//go:build darwin || linux

package v8runtime

import "github.com/ebitengine/purego"

type unixLibrary struct {
	handle uintptr
}

func openDynamicLibrary(path string) (dynamicLibrary, error) {
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	return &unixLibrary{handle: handle}, nil
}

func (library *unixLibrary) lookup(name string) (uintptr, error) {
	return purego.Dlsym(library.handle, name)
}

func (library *unixLibrary) close() error {
	return purego.Dlclose(library.handle)
}
