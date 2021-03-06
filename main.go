package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/golang/protobuf/protoc-gen-go/descriptor"
)

// defines several variables for parameterizing the protoc command. We can pull
// this out into a toml files in cases where we to vary this per package.
var (
	configPath string
	dryRun     bool
	quiet      bool
)

func init() {
	flag.StringVar(&configPath, "f", "Protobuild.toml", "override default config location")
	flag.BoolVar(&dryRun, "dryrun", false, "prints commands without running")
	flag.BoolVar(&quiet, "quiet", false, "suppress verbose output")
}

func main() {
	flag.Parse()

	c, err := readConfig(configPath)
	if err != nil {
		log.Fatalln(err)
	}

	pkgInfos, err := goPkgInfo(flag.Args()...)
	if err != nil {
		log.Fatalln(err)
	}

	gopath, err := gopathSrc()
	if err != nil {
		log.Fatalln(err)
	}

	gopathCurrent, err := gopathCurrent()
	if err != nil {
		log.Fatalln(err)
	}

	// For some reason, the golang protobuf generator makes the god awful
	// decision to output the files relative to the gopath root. It doesn't do
	// this only in the case where you give it ".".
	outputDir := filepath.Join(gopathCurrent, "src")

	// Index overrides by target import path
	overrides := map[string]struct {
		Prefixes  []string
		Generator string
		Plugins   *[]string
	}{}
	for _, override := range c.Overrides {
		for _, prefix := range override.Prefixes {
			overrides[prefix] = override
		}
	}

	descProto, err := descriptorProto(c.Includes.After)
	if err != nil {
		log.Fatalln(err)
	}

	// Aggregate descriptors for each descriptor prefix.
	descriptorSets := map[string]*descriptorSet{}
	for _, stable := range c.Descriptors {
		descriptorSets[stable.Prefix] = newDescriptorSet(stable.IgnoreFiles, descProto)
	}

	shouldGenerateDescriptors := func(p string) bool {
		for prefix := range descriptorSets {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}

		return false
	}

	var descriptors []*descriptor.FileDescriptorSet
	for _, pkg := range pkgInfos {
		var includes []string
		includes = append(includes, c.Includes.Before...)

		vendor, err := closestVendorDir(pkg.Dir)
		if err != nil {
			if err != errVendorNotFound {
				log.Fatalln(err)
			}
		}

		if vendor != "" {
			// TODO(stevvooe): The use of the closest vendor directory is a
			// little naive. We should probably resolve all possible vendor
			// directories or at least match Go's behavior.

			// we also special case the inclusion of gogoproto in the vendor dir.
			// We could parameterize this better if we find it to be a common case.
			var vendoredIncludesResolved []string
			for _, vendoredInclude := range c.Includes.Vendored {
				vendoredIncludesResolved = append(vendoredIncludesResolved,
					filepath.Join(vendor, vendoredInclude))
			}

			// Also do this for pkg includes.
			for _, pkgInclude := range c.Includes.Packages {
				vendoredIncludesResolved = append(vendoredIncludesResolved,
					filepath.Join(vendor, pkgInclude))
			}

			includes = append(includes, vendoredIncludesResolved...)
			includes = append(includes, vendor)
		} else if len(c.Includes.Vendored) > 0 {
			log.Println("ignoring vendored includes: vendor directory not found")
		}

		// handle packages that we want to have as an include root from any of
		// the gopaths.
		for _, pkg := range c.Includes.Packages {
			includes = append(includes, gopathJoin(gopath, pkg))
		}

		includes = append(includes, gopath)
		includes = append(includes, c.Includes.After...)

		protoc := protocCmd{
			Name:       c.Generator,
			ImportPath: pkg.GoImportPath,
			PackageMap: c.Packages,
			Plugins:    c.Plugins,
			Files:      pkg.ProtoFiles,
			OutputDir:  outputDir,
			Includes:   includes,
		}

		importDirPath, err := filepath.Rel(outputDir, pkg.Dir)
		if err != nil {
			log.Fatalln(err)
		}

		if override, ok := overrides[importDirPath]; ok {
			// selectively apply the overrides to the protoc structure.
			if override.Generator != "" {
				protoc.Name = override.Generator
			}

			if override.Plugins != nil {
				protoc.Plugins = *override.Plugins
			}
		}

		var (
			genDescriptors = shouldGenerateDescriptors(importDirPath)
			dfp            *os.File // tempfile for descriptors
		)

		if genDescriptors {
			dfp, err = ioutil.TempFile("", "descriptors.pb-")
			if err != nil {
				log.Fatalln(err)
			}
			protoc.Descriptors = dfp.Name()
		}

		arg, err := protoc.mkcmd()
		if err != nil {
			log.Fatalln(err)
		}

		if !quiet {
			fmt.Println(arg)
		}

		if dryRun {
			continue
		}

		if err := protoc.run(); err != nil {
			if quiet {
				log.Println(arg)
			}
			if err, ok := err.(*exec.ExitError); ok {
				if status, ok := err.Sys().(syscall.WaitStatus); ok {
					os.Exit(status.ExitStatus()) // proxy protoc exit status
				}
			}

			log.Fatalln(err)
		}

		if genDescriptors {
			desc, err := readDesc(protoc.Descriptors)
			if err != nil {
				log.Fatalln(err)
			}

			for path, set := range descriptorSets {
				if strings.HasPrefix(importDirPath, path) {
					set.add(desc.File...)
				}
			}
			descriptors = append(descriptors, desc)

			// clean up descriptors file
			if err := os.Remove(dfp.Name()); err != nil {
				log.Fatalln(err)
			}

			if err := dfp.Close(); err != nil {
				log.Fatalln(err)
			}
		}
	}

	for _, descriptorConfig := range c.Descriptors {
		fp, err := os.OpenFile(descriptorConfig.Target, os.O_TRUNC|os.O_WRONLY|os.O_CREATE, 0777)
		if err != nil {
			log.Fatalln(err)
		}
		defer fp.Sync()
		defer fp.Close()

		set := descriptorSets[descriptorConfig.Prefix]
		if len(set.merged.File) == 0 {
			continue // just skip if there is nothing.
		}

		if err := set.marshalTo(fp); err != nil {
			log.Fatalln(err)
		}
	}
}

type protoGoPkgInfo struct {
	Dir          string
	GoImportPath string
	ProtoFiles   []string
}

// goPkgInfo hunts down packages with proto files.
func goPkgInfo(golistpath ...string) ([]protoGoPkgInfo, error) {
	args := []string{
		"list", "-e", "-f", "{{.ImportPath}} {{.Dir}}"}
	args = append(args, golistpath...)
	cmd := exec.Command("go", args...)

	p, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var pkgInfos []protoGoPkgInfo
	lines := bytes.Split(p, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		parts := bytes.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad output from command: %s", p)
		}

		pkgInfo := protoGoPkgInfo{
			Dir:          string(parts[1]),
			GoImportPath: string(parts[0]),
		}

		protoFiles, err := filepath.Glob(filepath.Join(pkgInfo.Dir, "*.proto"))
		if err != nil {
			return nil, err
		}
		if len(protoFiles) == 0 {
			continue // not a proto directory, skip
		}

		pkgInfo.ProtoFiles = protoFiles
		pkgInfos = append(pkgInfos, pkgInfo)
	}

	return pkgInfos, nil
}

func gopaths() []string {
	gp := os.Getenv("GOPATH")

	if gp == "" {
		return nil
	}

	return strings.Split(gp, string(filepath.ListSeparator))
}

// gopathSrc modifies GOPATH elements from env to include the src directory.
func gopathSrc() (string, error) {
	gps := gopaths()
	if len(gps) == 0 {
		return "", fmt.Errorf("must be run from a gopath")
	}
	var elements []string
	for _, element := range gps {
		elements = append(elements, filepath.Join(element, "src"))
	}

	return strings.Join(elements, string(filepath.ListSeparator)), nil
}

// gopathCurrent provides the top-level gopath for the current generation.
func gopathCurrent() (string, error) {
	gps := gopaths()
	if len(gps) == 0 {
		return "", fmt.Errorf("must be run from a gopath")
	}

	return gps[0], nil
}

// gopathJoin combines the element with each path item and then recombines the set.
func gopathJoin(gopath, element string) string {
	gps := strings.Split(gopath, string(filepath.ListSeparator))
	var elements []string
	for _, p := range gps {
		elements = append(elements, filepath.Join(p, element))
	}

	return strings.Join(elements, string(filepath.ListSeparator))
}

// descriptorProto returns the full path to google/protobuf/descriptor.proto
// which might be different depending on whether it was installed. The argument
// is the list of paths to check.
func descriptorProto(paths []string) (string, error) {
	const descProto = "google/protobuf/descriptor.proto"

	for _, dir := range paths {
		file := path.Join(dir, descProto)
		if _, err := os.Stat(file); err == nil {
			return file, err
		}
	}

	return "", fmt.Errorf("File %q not found (looked in: %v)", descProto, paths)
}

var errVendorNotFound = fmt.Errorf("no vendor dir found")

// closestVendorDir walks up from dir until it finds the vendor directory.
func closestVendorDir(dir string) (string, error) {
	dir = filepath.Clean(dir)
	for dir != filepath.Join(filepath.VolumeName(dir), string(filepath.Separator)) {
		vendor := filepath.Join(dir, "vendor")
		fi, err := os.Stat(vendor)
		if err != nil {
			if os.IsNotExist(err) {
				// up we go!
				dir = filepath.Dir(dir)
				continue
			}
			return "", err
		}

		if !fi.IsDir() {
			// up we go!
			dir = filepath.Dir(dir)
			continue
		}

		return vendor, nil
	}

	return "", errVendorNotFound
}
