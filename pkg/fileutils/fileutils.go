package fileutils

import (
	log "github.com/Sirupsen/logrus"
	"path/filepath"
	"strings"
)

// Matches returns true if relFilePath matches any of the patterns
func Matches(relFilePath string, patterns []string) (bool, error) {
	for _, exclude := range patterns {
		matched, err := filepath.Match(exclude, relFilePath)
		if err != nil {
			log.Errorf("Error matching: %s (pattern: %s)", relFilePath, exclude)
			return false, err
		}
		if matched {
			if filepath.Clean(relFilePath) == "." {
				log.Errorf("Can't exclude whole path, excluding pattern: %s", exclude)
				continue
			}
			log.Debugf("Skipping excluded path: %s (pattern %s)", relFilePath, exclude)
			return true, nil
		}
	}
	return false, nil
}

// RecursiveMatches return true if relFilePath(or it's parent) (recursively)matches any of the patterns
// or patterns is empty.
func RecursiveMatches(relFilePath string, patterns map[string][]string) (bool, error) {
	if len(patterns) <= 0 {
		return true, nil
	}
	for _, paths := range patterns {
		for _, include := range paths {
			matched, err := filepath.Match(include, relFilePath)
			if err != nil {
				log.Errorf("Error matching: %s (pattern: %s)", relFilePath, include)
				return false, err
			}
			if matched {
				log.Debugf("Matched included path: %s (pattern: %s)", relFilePath, include)
				return true, nil
			}
		}
	}
	// match fails
	if relFilePath == "/" {
		return false, nil
	}
	recursivePatterns := make(map[string][]string)
	for path, _ := range patterns {
		recursivePatterns[path] = append(recursivePatterns[path], path)
	}
	return RecursiveMatches(filepath.Dir(relFilePath), recursivePatterns)
}

func MapFilePaths(filePaths []string) map[string][]string {
	object := make(map[string][]string)
	for _, path := range filePaths {
		base := "/"
		for _, part := range strings.Split(path, "/") {
			if part != "" {
				base = filepath.Join(base, part)
				object[path] = append(object[path], base)
			}
		}
	}
	return object
}
