package v8runtime

import "syscall"

type windowsLibrary struct {
	handle syscall.Handle
}

func openDynamicLibrary(path string) (dynamicLibrary, error) {
	handle, err := syscall.LoadLibrary(path)
	if err != nil {
		return nil, err
	}
	return &windowsLibrary{handle: handle}, nil
}

func (library *windowsLibrary) lookup(name string) (uintptr, error) {
	return syscall.GetProcAddress(library.handle, name)
}

func (library *windowsLibrary) close() error {
	return syscall.FreeLibrary(library.handle)
}
