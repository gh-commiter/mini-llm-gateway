package main

import "os"

// MXC and agent tool rules take paths, not directory handles.
func openCheckedFolder(f checkedFolder) (*os.File, error) {
	return nil, (checkedFolders{f}).verify()
}

func pinAllowedBinds(args []string, folders checkedFolders, held []*os.File) ([]string, []*os.File) {
	return args, nil
}

func allowedClaudeWrap(cfgDir, name string, args []string, folders checkedFolders, held []*os.File, readOnly bool, logf func(string, ...any)) (string, []string, []*os.File) {
	return name, args, nil
}
