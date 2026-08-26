// Package filemgr provides remote directory exploration and secure chunked file transfer
// for the Windows agent.
package filemgr

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"terminalagent/internal/protocol"
)

// MaxFileTransferBytes is the safety limit for single-file transfers (50 MB).
const MaxFileTransferBytes = 50 * 1024 * 1024

// DefaultRoot returns the default root directory for exploration.
func DefaultRoot() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("SystemDrive"); v != "" {
			return v + "\\"
		}
		return "C:\\"
	}
	return "/"
}

// ListDirectory returns the list of files and folders inside dirPath.
func ListDirectory(dirPath string) ([]protocol.FileInfo, error) {
	if dirPath == "" || dirPath == "." {
		dirPath = DefaultRoot()
	}

	// If user specifically requests "DRIVES" or root drives list on Windows
	if (dirPath == "DRIVES" || dirPath == "ROOT") && runtime.GOOS == "windows" {
		return listWindowsDrivesAndQuickDirs()
	}

	cleanPath := filepath.Clean(dirPath)
	// Ensure Windows drive root has trailing backslash e.g. "C:\" instead of "C:"
	if len(cleanPath) == 2 && cleanPath[1] == ':' {
		cleanPath += "\\"
	}

	entries, err := os.ReadDir(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("read directory %q: %w", cleanPath, err)
	}

	var files []protocol.FileInfo
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		fullPath := filepath.Join(cleanPath, entry.Name())
		files = append(files, protocol.FileInfo{
			Name:      entry.Name(),
			Path:      fullPath,
			SizeBytes: info.Size(),
			IsDir:     entry.IsDir(),
			ModTime:   info.ModTime().Unix(),
			Mode:      info.Mode().String(),
		})
	}

	// Sort directories first, then alphabetical by name
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})

	return files, nil
}

func listWindowsDrivesAndQuickDirs() ([]protocol.FileInfo, error) {
	var list []protocol.FileInfo

	// Check drive letters A-Z
	for d := 'C'; d <= 'Z'; d++ {
		drivePath := string(d) + ":\\"
		if _, err := os.Stat(drivePath); err == nil {
			list = append(list, protocol.FileInfo{
				Name:  fmt.Sprintf("Local Disk (%c:)", d),
				Path:  drivePath,
				IsDir: true,
			})
		}
	}

	// Add User folders
	if home, err := os.UserHomeDir(); err == nil {
		quickDirs := []struct {
			name string
			sub  string
		}{
			{"Desktop", "Desktop"},
			{"Downloads", "Downloads"},
			{"Documents", "Documents"},
			{"User Home", ""},
		}
		for _, q := range quickDirs {
			p := filepath.Join(home, q.sub)
			if _, err := os.Stat(p); err == nil {
				list = append(list, protocol.FileInfo{
					Name:  "📁 " + q.name,
					Path:  p,
					IsDir: true,
				})
			}
		}
	}

	return list, nil
}

// ReadFile reads the target file and returns base64-encoded content.
func ReadFile(filePath string) (string, int64, error) {
	cleanPath := filepath.Clean(filePath)
	info, err := os.Stat(cleanPath)
	if err != nil {
		return "", 0, fmt.Errorf("file not found: %w", err)
	}
	if info.IsDir() {
		return "", 0, errors.New("cannot read a directory as a file")
	}
	if info.Size() > MaxFileTransferBytes {
		return "", 0, fmt.Errorf("file size (%d bytes) exceeds maximum transfer limit (%d bytes)", info.Size(), MaxFileTransferBytes)
	}

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", 0, fmt.Errorf("read file: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	return encoded, info.Size(), nil
}

// WriteFile decodes base64 content and writes it to target filePath.
func WriteFile(filePath, contentBase64 string, overwrite bool) error {
	cleanPath := filepath.Clean(filePath)

	if !overwrite {
		if _, err := os.Stat(cleanPath); err == nil {
			return fmt.Errorf("file already exists and overwrite is false: %s", cleanPath)
		}
	}

	data, err := base64.StdEncoding.DecodeString(contentBase64)
	if err != nil {
		return fmt.Errorf("invalid base64 content: %w", err)
	}

	dir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	if err := os.WriteFile(cleanPath, data, 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

// DeleteFile removes a file or directory.
func DeleteFile(path string) error {
	cleanPath := filepath.Clean(path)
	if _, err := os.Stat(cleanPath); os.IsNotExist(err) {
		return fmt.Errorf("path does not exist: %s", cleanPath)
	}
	return os.RemoveAll(cleanPath)
}
