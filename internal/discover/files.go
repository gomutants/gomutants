package discover

import (
	"os"
	"path/filepath"
)

// readFileBytesFunc is swappable for testing.
var readFileBytesFunc = os.ReadFile

func readFileBytes(path string) ([]byte, error) {
	return readFileBytesFunc(path)
}

// PreReadFiles reads all source files from the given packages into memory.
// Returns a map from absolute path to file contents.
//
// A file already in parsed (Discover's parse cache) contributes the bytes
// it was parsed from rather than a fresh read. Mutant offsets were computed
// against those bytes, so overlays built from them patch the right place
// even if the file changed on disk since discovery. Only files Discover
// skipped are read from disk.
func PreReadFiles(pkgs []Package, parsed map[string]*ParsedFile) (map[string][]byte, error) {
	files := make(map[string][]byte)
	for _, pkg := range pkgs {
		for _, filename := range pkg.GoFiles {
			absPath := filepath.Join(pkg.Dir, filename)
			if _, ok := files[absPath]; ok {
				continue
			}
			if pf, ok := parsed[absPath]; ok {
				files[absPath] = pf.Src
				continue
			}
			data, err := readFileBytesFunc(absPath)
			if err != nil {
				return nil, err
			}
			files[absPath] = data
		}
	}
	return files, nil
}
