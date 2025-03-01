/*
   Copyright © 2021 The CDI Authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package cdi

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"

	cdi "github.com/container-orchestrated-devices/container-device-interface/specs-go"
	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/go-multierror"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

// Option is an option to change some aspect of default CDI behavior.
type Option func(*Cache) error

// Cache stores CDI Specs loaded from Spec directories.
type Cache struct {
	sync.Mutex
	specDirs  []string
	specs     map[string][]*Spec
	devices   map[string]*Device
	errors    map[string][]error
	dirErrors map[string]error

	autoRefresh bool
	watch       *watch
}

// WithAutoRefresh returns an option to control automatic Cache refresh.
// By default auto-refresh is enabled, the list of Spec directories are
// monitored and the Cache is automatically refreshed whenever a change
// is detected. This option can be used to disable this behavior when a
// manually refreshed mode is preferable.
func WithAutoRefresh(autoRefresh bool) Option {
	return func(c *Cache) error {
		c.autoRefresh = autoRefresh
		return nil
	}
}

// NewCache creates a new CDI Cache. The cache is populated from a set
// of CDI Spec directories. These can be specified using a WithSpecDirs
// option. The default set of directories is exposed in DefaultSpecDirs.
func NewCache(options ...Option) (*Cache, error) {
	c := &Cache{
		autoRefresh: true,
		watch:       &watch{},
	}

	WithSpecDirs(DefaultSpecDirs...)(c)
	c.Lock()
	defer c.Unlock()

	return c, c.configure(options...)
}

// Configure applies options to the Cache. Updates and refreshes the
// Cache if options have changed.
func (c *Cache) Configure(options ...Option) error {
	if len(options) == 0 {
		return nil
	}

	c.Lock()
	defer c.Unlock()

	return c.configure(options...)
}

// Configure the Cache. Start/stop CDI Spec directory watch, refresh
// the Cache if necessary.
func (c *Cache) configure(options ...Option) error {
	var err error

	for _, o := range options {
		if err = o(c); err != nil {
			return errors.Wrapf(err, "failed to apply cache options")
		}
	}

	c.dirErrors = make(map[string]error)

	c.watch.stop()
	if c.autoRefresh {
		c.watch.setup(c.specDirs, c.dirErrors)
		c.watch.start(&c.Mutex, c.refresh, c.dirErrors)
	}
	c.refresh()

	return nil
}

// Refresh rescans the CDI Spec directories and refreshes the Cache.
// In manual refresh mode the cache is always refreshed. In auto-
// refresh mode the cache is only refreshed if it is out of date.
func (c *Cache) Refresh() error {
	c.Lock()
	defer c.Unlock()

	// force a refresh in manual mode
	if refreshed, err := c.refreshIfRequired(!c.autoRefresh); refreshed {
		return err
	}

	// collect and return cached errors, much like refresh() does it
	var result error
	for _, err := range c.errors {
		result = multierror.Append(result, err...)
	}
	return result
}

// Refresh the Cache by rescanning CDI Spec directories and files.
func (c *Cache) refresh() error {
	var (
		specs      = map[string][]*Spec{}
		devices    = map[string]*Device{}
		conflicts  = map[string]struct{}{}
		specErrors = map[string][]error{}
		result     []error
	)

	// collect errors per spec file path and once globally
	collectError := func(err error, paths ...string) {
		result = append(result, err)
		for _, path := range paths {
			specErrors[path] = append(specErrors[path], err)
		}
	}
	// resolve conflicts based on device Spec priority (order of precedence)
	resolveConflict := func(name string, dev *Device, old *Device) bool {
		devSpec, oldSpec := dev.GetSpec(), old.GetSpec()
		devPrio, oldPrio := devSpec.GetPriority(), oldSpec.GetPriority()
		switch {
		case devPrio > oldPrio:
			return false
		case devPrio == oldPrio:
			devPath, oldPath := devSpec.GetPath(), oldSpec.GetPath()
			collectError(errors.Errorf("conflicting device %q (specs %q, %q)",
				name, devPath, oldPath), devPath, oldPath)
			conflicts[name] = struct{}{}
		}
		return true
	}

	_ = scanSpecDirs(c.specDirs, func(path string, priority int, spec *Spec, err error) error {
		path = filepath.Clean(path)
		if err != nil {
			collectError(errors.Wrapf(err, "failed to load CDI Spec"), path)
			return nil
		}

		vendor := spec.GetVendor()
		specs[vendor] = append(specs[vendor], spec)

		for _, dev := range spec.devices {
			qualified := dev.GetQualifiedName()
			other, ok := devices[qualified]
			if ok {
				if resolveConflict(qualified, dev, other) {
					continue
				}
			}
			devices[qualified] = dev
		}

		return nil
	})

	for conflict := range conflicts {
		delete(devices, conflict)
	}

	c.specs = specs
	c.devices = devices
	c.errors = specErrors

	if len(result) > 0 {
		return multierror.Append(nil, result...)
	}

	return nil
}

// RefreshIfRequired triggers a refresh if necessary.
func (c *Cache) refreshIfRequired(force bool) (bool, error) {
	// We need to refresh if
	// - it's forced by an explicitly call to Refresh() in manual mode
	// - a missing Spec dir appears (added to watch) in auto-refresh mode
	if force || (c.autoRefresh && c.watch.update(c.dirErrors)) {
		return true, c.refresh()
	}
	return false, nil
}

// InjectDevices injects the given qualified devices to an OCI Spec. It
// returns any unresolvable devices and an error if injection fails for
// any of the devices.
func (c *Cache) InjectDevices(ociSpec *oci.Spec, devices ...string) ([]string, error) {
	var unresolved []string

	if ociSpec == nil {
		return devices, errors.Errorf("can't inject devices, nil OCI Spec")
	}

	c.Lock()
	defer c.Unlock()

	c.refreshIfRequired(false)

	edits := &ContainerEdits{}
	specs := map[*Spec]struct{}{}

	for _, device := range devices {
		d := c.devices[device]
		if d == nil {
			unresolved = append(unresolved, device)
			continue
		}
		if _, ok := specs[d.GetSpec()]; !ok {
			specs[d.GetSpec()] = struct{}{}
			edits.Append(d.GetSpec().edits())
		}
		edits.Append(d.edits())
	}

	if unresolved != nil {
		return unresolved, errors.Errorf("unresolvable CDI devices %s",
			strings.Join(devices, ", "))
	}

	if err := edits.Apply(ociSpec); err != nil {
		return nil, errors.Wrap(err, "failed to inject devices")
	}

	return nil, nil
}

// WriteSpec writes a Spec file with the given content. Priority is used
// as an index into the list of Spec directories to pick a directory for
// the file, adjusting for any under- or overflows. If name has a "json"
// or "yaml" extension it choses the encoding. Otherwise JSON encoding
// is used with a "json" extension.
func (c *Cache) WriteSpec(raw *cdi.Spec, name string) error {
	var (
		specDir string
		path    string
		prio    int
		spec    *Spec
		err     error
	)

	if len(c.specDirs) == 0 {
		return errors.New("no Spec directories to write to")
	}

	prio = len(c.specDirs) - 1
	specDir = c.specDirs[prio]
	path = filepath.Join(specDir, name)
	if ext := filepath.Ext(path); ext != ".json" && ext != ".yaml" {
		path += ".json"
	}

	spec, err = NewSpec(raw, path, prio)
	if err != nil {
		return err
	}

	return spec.Write(true)
}

// GetDevice returns the cached device for the given qualified name.
func (c *Cache) GetDevice(device string) *Device {
	c.Lock()
	defer c.Unlock()

	c.refreshIfRequired(false)

	return c.devices[device]
}

// ListDevices lists all cached devices by qualified name.
func (c *Cache) ListDevices() []string {
	var devices []string

	c.Lock()
	defer c.Unlock()

	c.refreshIfRequired(false)

	for name := range c.devices {
		devices = append(devices, name)
	}
	sort.Strings(devices)

	return devices
}

// ListVendors lists all vendors known to the cache.
func (c *Cache) ListVendors() []string {
	var vendors []string

	c.Lock()
	defer c.Unlock()

	c.refreshIfRequired(false)

	for vendor := range c.specs {
		vendors = append(vendors, vendor)
	}
	sort.Strings(vendors)

	return vendors
}

// ListClasses lists all device classes known to the cache.
func (c *Cache) ListClasses() []string {
	var (
		cmap    = map[string]struct{}{}
		classes []string
	)

	c.Lock()
	defer c.Unlock()

	c.refreshIfRequired(false)

	for _, specs := range c.specs {
		for _, spec := range specs {
			cmap[spec.GetClass()] = struct{}{}
		}
	}
	for class := range cmap {
		classes = append(classes, class)
	}
	sort.Strings(classes)

	return classes
}

// GetVendorSpecs returns all specs for the given vendor.
func (c *Cache) GetVendorSpecs(vendor string) []*Spec {
	c.Lock()
	defer c.Unlock()

	c.refreshIfRequired(false)

	return c.specs[vendor]
}

// GetSpecErrors returns all errors encountered for the spec during the
// last cache refresh.
func (c *Cache) GetSpecErrors(spec *Spec) []error {
	return c.errors[spec.GetPath()]
}

// GetErrors returns all errors encountered during the last
// cache refresh.
func (c *Cache) GetErrors() map[string][]error {
	c.Lock()
	defer c.Unlock()

	errors := map[string][]error{}
	for path, errs := range c.errors {
		errors[path] = errs
	}
	for path, err := range c.dirErrors {
		errors[path] = []error{err}
	}

	return errors
}

// GetSpecDirectories returns the CDI Spec directories currently in use.
func (c *Cache) GetSpecDirectories() []string {
	c.Lock()
	defer c.Unlock()

	dirs := make([]string, len(c.specDirs))
	copy(dirs, c.specDirs)
	return dirs
}

// GetSpecDirErrors returns any errors related to configured Spec directories.
func (c *Cache) GetSpecDirErrors() map[string]error {
	if c.dirErrors == nil {
		return nil
	}

	c.Lock()
	defer c.Unlock()

	errors := make(map[string]error)
	for dir, err := range c.dirErrors {
		errors[dir] = err
	}
	return errors
}

// Our fsnotify helper wrapper.
type watch struct {
	watcher *fsnotify.Watcher
	tracked map[string]bool
}

// Setup monitoring for the given Spec directories.
func (w *watch) setup(dirs []string, dirErrors map[string]error) {
	var (
		dir string
		err error
	)
	w.tracked = make(map[string]bool)
	for _, dir = range dirs {
		w.tracked[dir] = false
	}

	w.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		for _, dir := range dirs {
			dirErrors[dir] = errors.Wrap(err, "failed to create watcher")
		}
		return
	}

	w.update(dirErrors)
}

// Start watching Spec directories for relevant changes.
func (w *watch) start(m *sync.Mutex, refresh func() error, dirErrors map[string]error) {
	go w.watch(m, refresh, dirErrors)
}

// Stop watching directories.
func (w *watch) stop() {
	if w.watcher == nil {
		return
	}

	w.watcher.Close()
	w.tracked = nil
}

// Watch Spec directory changes, triggering a refresh if necessary.
func (w *watch) watch(m *sync.Mutex, refresh func() error, dirErrors map[string]error) {
	watch := w.watcher
	if watch == nil {
		return
	}
	for {
		select {
		case event, ok := <-watch.Events:
			if !ok {
				return
			}

			if (event.Op & (fsnotify.Rename | fsnotify.Remove | fsnotify.Write)) == 0 {
				continue
			}
			if event.Op == fsnotify.Write {
				if ext := filepath.Ext(event.Name); ext != ".json" && ext != ".yaml" {
					continue
				}
			}

			m.Lock()
			if event.Op == fsnotify.Remove && w.tracked[event.Name] {
				w.update(dirErrors, event.Name)
			} else {
				w.update(dirErrors)
			}
			refresh()
			m.Unlock()

		case _, ok := <-watch.Errors:
			if !ok {
				return
			}
		}
	}
}

// Update watch with pending/missing or removed directories.
func (w *watch) update(dirErrors map[string]error, removed ...string) bool {
	var (
		dir    string
		ok     bool
		err    error
		update bool
	)

	for dir, ok = range w.tracked {
		if ok {
			continue
		}

		err = w.watcher.Add(dir)
		if err == nil {
			w.tracked[dir] = true
			delete(dirErrors, dir)
			update = true
		} else {
			w.tracked[dir] = false
			dirErrors[dir] = errors.Wrap(err, "failed to monitor for changes")
		}
	}

	for _, dir = range removed {
		w.tracked[dir] = false
		dirErrors[dir] = errors.New("directory removed")
		update = true
	}

	return update
}
