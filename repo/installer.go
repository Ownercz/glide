package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Masterminds/glide/cfg"
	"github.com/Masterminds/glide/dependency"
	"github.com/Masterminds/glide/importer"
	"github.com/Masterminds/glide/msg"
	gpath "github.com/Masterminds/glide/path"
	"github.com/Masterminds/glide/util"
	"github.com/codegangsta/cli"
)

// Installer provides facilities for installing the repos in a config file.
type Installer struct {

	// Force the install when certain normally stopping conditions occur.
	Force bool

	// Home is the location of cache
	Home string

	// Vendor contains the path to put the vendor packages
	Vendor string

	// Use a cache
	UseCache bool
	// Use Gopath to cache
	UseCacheGopath bool
	// Use Gopath as a source to read from
	UseGopath bool

	// UpdateVendored instructs the environment to update in a way that is friendly
	// to packages that have been "vendored in" (e.g. are copies of source, not repos)
	UpdateVendored bool

	// DeleteUnused deletes packages that are unused, but found in the vendor dir.
	DeleteUnused bool

	// RootPackage is the top level package importing other packages. If an
	// imported pacakgage references this pacakage it does not need to be
	// downloaded and searched out again.
	RootPackage string

	// An instance of *cfg.Config that's safe to modify. This should be a deep
	// clone of the Config instance holding the config of record.
	Config *cfg.Config
}

// VendorPath returns the path to the location to put vendor packages
func (i *Installer) VendorPath() string {
	if i.Vendor != "" {
		return i.Vendor
	}

	vp, err := gpath.Vendor()
	if err != nil {
		return filepath.FromSlash("./vendor")
	}

	return vp
}

// Install installs the dependencies from a Lockfile.
func (i *Installer) Install(lock *cfg.Lockfile, conf *cfg.Config) (*cfg.Config, error) {

	cwd, err := gpath.Vendor()
	if err != nil {
		return conf, err
	}

	// Create a config setup based on the Lockfile data to process with
	// existing commands.
	newConf := &cfg.Config{}
	newConf.Name = conf.Name

	newConf.Imports = make(cfg.Dependencies, len(lock.Imports))
	for k, v := range lock.Imports {
		newConf.Imports[k] = &cfg.Dependency{
			Name:        v.Name,
			Reference:   v.Version,
			Repository:  v.Repository,
			VcsType:     v.VcsType,
			Subpackages: v.Subpackages,
			Arch:        v.Arch,
			Os:          v.Os,
		}
	}

	newConf.DevImports = make(cfg.Dependencies, len(lock.DevImports))
	for k, v := range lock.DevImports {
		newConf.DevImports[k] = &cfg.Dependency{
			Name:        v.Name,
			Reference:   v.Version,
			Repository:  v.Repository,
			VcsType:     v.VcsType,
			Subpackages: v.Subpackages,
			Arch:        v.Arch,
			Os:          v.Os,
		}
	}

	newConf.DeDupe()

	if len(newConf.Imports) == 0 {
		msg.Info("No dependencies found. Nothing installed.\n")
		return newConf, nil
	}

	ConcurrentUpdate(newConf.Imports, cwd, i)
	ConcurrentUpdate(newConf.DevImports, cwd, i)
	return newConf, nil
}

// Checkout reads the config file and checks out all dependencies mentioned there.
//
// This is used when initializing an empty vendor directory, or when updating a
// vendor directory based on changed config.
func (i *Installer) Checkout(conf *cfg.Config, useDev bool) error {

	dest := i.VendorPath()

	if err := ConcurrentUpdate(conf.Imports, dest, i); err != nil {
		return err
	}

	if useDev {
		return ConcurrentUpdate(conf.DevImports, dest, i)
	}

	return nil
}

// Update updates all dependencies.
//
// It begins with the dependencies in the config file, but also resolves
// transitive dependencies. The returned lockfile has all of the dependencies
// listed, but the version reconciliation has not been done.
//
// In other words, all versions in the Lockfile will be empty.
func (i *Installer) Update(conf *cfg.Config) error {
	base := "."
	vpath := i.VendorPath()

	m := &MissingPackageHandler{
		destination: vpath,

		cache:       i.UseCache,
		cacheGopath: i.UseCacheGopath,
		useGopath:   i.UseGopath,
		home:        i.Home,
		Config:      conf,
	}

	v := &VersionHandler{
		Destination: vpath,
		Deps:        make(map[string]*cfg.Dependency),
		Use:         make(map[string]*cfg.Dependency),
		Imported:    make(map[string]bool),
		Conflicts:   make(map[string]bool),
		Config:      conf,
	}

	// Update imports
	res, err := dependency.NewResolver(base)
	if err != nil {
		msg.Die("Failed to create a resolver: %s", err)
	}
	res.Config = conf
	res.Handler = m
	res.VersionHandler = v
	msg.Info("Resolving imports")
	packages, err := allPackages(conf.Imports, res)
	if err != nil {
		msg.Die("Failed to retrieve a list of dependencies: %s", err)
	}

	msg.Warn("devImports not resolved.")

	deps := depsFromPackages(packages)

	// TODO(mattfarina): We need to not go back and forth between between
	// paths and cfg.Dependency instances.
	// If we have conf.Imports we copy them to the final list to pull up elements
	// like the VCS information.
	for k, d := range deps {
		for _, dep := range conf.Imports {
			if dep.Name == d.Name {
				deps[k] = dep
			}
		}
	}

	// Copy over the dependency information from the version system which contains
	// pinned information, VCS info, etc.
	for _, d := range deps {
		d2, found := v.Deps[d.Name]
		if found {
			d.Pin = d2.Pin
			if d.Repository == "" {
				d.Repository = d2.Repository
			}
			if d.VcsType == "" {
				d.VcsType = d2.VcsType
			}
			if d.Reference == "" {
				d.Reference = d2.Reference
			}
		}
	}

	err = ConcurrentUpdate(deps, vpath, i)
	conf.Imports = deps

	return err
}

func (i *Installer) List(conf *cfg.Config) []*cfg.Dependency {
	base := "."
	vpath := i.VendorPath()

	v := &VersionHandler{
		Destination: vpath,
		Deps:        make(map[string]*cfg.Dependency),
		Use:         make(map[string]*cfg.Dependency),
		Imported:    make(map[string]bool),
		Conflicts:   make(map[string]bool),
		Config:      conf,
	}

	// Update imports
	res, err := dependency.NewResolver(base)
	if err != nil {
		msg.Die("Failed to create a resolver: %s", err)
	}
	res.Config = conf
	res.VersionHandler = v

	msg.Info("Resolving imports")
	packages, err := allPackages(conf.Imports, res)
	if err != nil {
		msg.Die("Failed to retrieve a list of dependencies: %s", err)
	}
	deps := depsFromPackages(packages)

	// TODO(mattfarina): We need to not go back and forth between between
	// paths and cfg.Dependency instances.
	// If we have conf.Imports we copy them to the final list to pull up elements
	// like the VCS information.
	for k, d := range deps {
		for _, dep := range conf.Imports {
			if dep.Name == d.Name {
				deps[k] = dep
			}
		}
	}

	// Copy over the dependency information from the version system which contains
	// pinned information, VCS info, etc.
	for _, d := range deps {
		d2, found := v.Deps[d.Name]
		if found {
			d.Pin = d2.Pin
			if d.Repository == "" {
				d.Repository = d2.Repository
			}
			if d.VcsType == "" {
				d.VcsType = d2.VcsType
			}
			if d.Reference == "" {
				d.Reference = d2.Reference
			}
		}
	}
	conf.Imports = deps

	msg.Warn("devImports not resolved.")

	return deps
}

// ConcurrentUpdate takes a list of dependencies and updates in parallel.
func ConcurrentUpdate(deps []*cfg.Dependency, cwd string, i *Installer) error {
	done := make(chan struct{}, concurrentWorkers)
	in := make(chan *cfg.Dependency, concurrentWorkers)
	var wg sync.WaitGroup
	var lock sync.Mutex
	var returnErr error

	for ii := 0; ii < concurrentWorkers; ii++ {
		go func(ch <-chan *cfg.Dependency) {
			for {
				select {
				case dep := <-ch:
					if err := VcsUpdate(dep, cwd, i); err != nil {
						msg.Warn("Update failed for %s: %s\n", dep.Name, err)
						// Capture the error while making sure the concurrent
						// operations don't step on each other.
						lock.Lock()
						if returnErr == nil {
							returnErr = err
						} else {
							returnErr = cli.NewMultiError(returnErr, err)
						}
						lock.Unlock()
					}
					wg.Done()
				case <-done:
					return
				}
			}
		}(in)
	}

	for _, dep := range deps {
		wg.Add(1)
		in <- dep
	}

	wg.Wait()

	// Close goroutines setting the version
	for ii := 0; ii < concurrentWorkers; ii++ {
		done <- struct{}{}
	}

	return returnErr
}

// allPackages gets a list of all packages required to satisfy the given deps.
func allPackages(deps []*cfg.Dependency, res *dependency.Resolver) ([]string, error) {
	if len(deps) == 0 {
		return []string{}, nil
	}

	vdir, err := gpath.Vendor()
	if err != nil {
		return []string{}, err
	}
	vdir += string(os.PathSeparator)
	ll, err := res.ResolveAll(deps)
	if err != nil {
		return []string{}, err
	}

	for i := 0; i < len(ll); i++ {
		ll[i] = strings.TrimPrefix(ll[i], vdir)
	}
	return ll, nil
}

/* unused
func reposFromPackages(pkgs []string) []string {
	// Make sure we don't have to resize this.
	seen := make(map[string]bool, len(pkgs))

	// Order is important.
	repos := []string{}

	for _, p := range pkgs {
		rr, _ := util.NormalizeName(p)
		if !seen[rr] {
			seen[rr] = true
			repos = append(repos, rr)
		}
	}
	return repos
}
*/

func depsFromPackages(pkgs []string) []*cfg.Dependency {
	// Make sure we don't have to resize this.
	seen := make(map[string]*cfg.Dependency, len(pkgs))

	// Order is important.
	deps := []*cfg.Dependency{}

	for _, p := range pkgs {
		rr, sp := util.NormalizeName(p)
		if _, ok := seen[rr]; !ok {
			subpkg := []string{}
			if sp != "" {
				subpkg = append(subpkg, sp)
			}

			dd := &cfg.Dependency{
				Name:        rr,
				Subpackages: subpkg,
			}

			deps = append(deps, dd)
			seen[rr] = dd
		} else if sp != "" {
			seen[rr].Subpackages = append(seen[rr].Subpackages, sp)
		}
	}
	return deps
}

// MissingPackageHandler is a dependency.MissingPackageHandler.
//
// When a package is not found, this attempts to resolve and fetch.
//
// When a package is found on the GOPATH, this notifies the user.
type MissingPackageHandler struct {
	destination                   string
	home                          string
	cache, cacheGopath, useGopath bool
	RootPackage                   string
	Config                        *cfg.Config
}

func (m *MissingPackageHandler) NotFound(pkg string) (bool, error) {
	root := util.GetRootFromPackage(pkg)

	// Skip any references to the root package.
	if root == m.RootPackage {
		return false, nil
	}
	if m.Config.HasIgnore(root) || m.Config.HasIgnore(pkg) {
		return false, nil
	}

	dest := filepath.Join(m.destination, root)

	// This package may have been placed on the list to look for when it wasn't
	// downloaded but it has since been downloaded before coming to this entry.
	if _, err := os.Stat(dest); err == nil {
		return true, nil
	}

	msg.Info("Fetching %s into %s", pkg, m.destination)

	d := &cfg.Dependency{Name: root}
	if err := VcsGet(d, dest, m.home, m.cache, m.cacheGopath, m.useGopath); err != nil {
		return false, err
	}
	return true, nil
}

func (m *MissingPackageHandler) OnGopath(pkg string) (bool, error) {
	// If useGopath is false, we fall back to the strategy of fetching from
	// remote.
	if !m.useGopath {
		return m.NotFound(pkg)
	}

	root := util.GetRootFromPackage(pkg)

	// Skip any references to the root package.
	if root == m.RootPackage {
		return false, nil
	}
	if m.Config.HasIgnore(root) || m.Config.HasIgnore(pkg) {
		return false, nil
	}

	msg.Info("Copying package %s from the GOPATH.", pkg)
	dest := filepath.Join(m.destination, pkg)
	// Find package on Gopath
	for _, gp := range gpath.Gopaths() {
		src := filepath.Join(gp, pkg)
		// FIXME: Should probably check if src is a dir or symlink.
		if _, err := os.Stat(src); err == nil {
			if err := os.MkdirAll(dest, os.ModeDir|0755); err != nil {
				return false, err
			}
			if err := gpath.CopyDir(src, dest); err != nil {
				return false, err
			}
			return true, nil
		}
	}

	msg.Error("Could not locate %s on the GOPATH, though it was found before.", pkg)
	return false, nil
}

// VersionHandler handles setting the proper version in the VCS.
type VersionHandler struct {

	// Deps provides a map of packages and their dependency instances.
	Deps map[string]*cfg.Dependency

	// If Try to use the version here if we have one. This is a cache and will
	// change over the course of setting versions.
	Use map[string]*cfg.Dependency

	// Cache if importing scan has already occured here.
	Imported map[string]bool

	// Where the packages exist to set the version on.
	Destination string

	RootPackage string
	Config      *cfg.Config

	// There's a problem where many sub-packages have been asked to set a version
	// and you can end up with numerous conflict messages that are exactly the
	// same. We are keeping track to only display them once.
	// the parent pac
	Conflicts map[string]bool
}

// SetVersion sets the version for a package. If that package version is already
// set it handles the case by:
// - keeping the already set version
// - proviting messaging about the version conflict
func (d *VersionHandler) SetVersion(pkg string) (e error) {
	root := util.GetRootFromPackage(pkg)

	// Skip any references to the root package.
	if root == d.RootPackage {
		return nil
	}
	if d.Config.HasIgnore(root) || d.Config.HasIgnore(pkg) {
		return nil
	}

	v, found := d.Deps[root]

	// We have not tried to import, yet.
	// Should we look in places other than the root of the project?
	if d.Imported[root] == false {
		d.Imported[root] = true
		p := filepath.Join(d.Destination, root)
		f, deps, err := importer.Import(p)
		if f && err == nil {

			// Store the imported version information. This will overwrite
			// previous entries. The latest imported is the version to use when
			// something is not pinned already. Once a version is set and pinned
			// it will not be changed later. So, the first to set the version
			// wins.
			for _, dep := range deps {
				if dep.Reference != "" {
					d.Use[dep.Name] = dep
				}
			}
		} else if err != nil {
			msg.Error("Unable to import from %s. Err: %s", root, err)
			e = err
		}
	}

	// If we are already pinned provide some useful messaging.
	if found {
		msg.Debug("Package %s is already pinned to %q", pkg, v.Pin)

		// Catch requested version conflicts here.
		if d.Use[root].Reference != "" && d.Use[root].Reference != d.Deps[root].Pin &&
			d.Use[root].Reference != d.Deps[root].Reference {
			s := fmt.Sprintf("Conflict: %s version is %s, but also asked for %s\n", root, d.Deps[root].Pin, d.Use[root].Reference)
			if !d.Conflicts[s] {
				d.Conflicts[s] = true
				msg.Warn(s)
			}
		}

		return
	}

	// The first time we've encountered this so try to set the version.
	dep, found := d.Use[root]
	if !found {
		msg.Debug("Unable to set version on %s, version to set unknown", root)
		return
	}
	err := VcsVersion(dep, d.Destination)
	if err != nil {
		msg.Warn("Unable to set verion on %s to %s. Err: ", root, dep.Reference, err)
		e = err
	}
	d.Deps[root] = dep
	return
}
