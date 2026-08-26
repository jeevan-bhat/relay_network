package filemgr_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"terminalagent/internal/filemgr"
)

func TestFileOperations(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Write file
	targetFile := filepath.Join(tempDir, "subfolder", "test.txt")
	testData := "Hello from remote terminal mobile test!"
	encodedData := base64.StdEncoding.EncodeToString([]byte(testData))

	if err := filemgr.WriteFile(targetFile, encodedData, false); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Verify cannot overwrite when overwrite = false
	if err := filemgr.WriteFile(targetFile, encodedData, false); err == nil {
		t.Fatal("expected error on duplicate write when overwrite is false")
	}

	// 2. Read file
	readContent, size, err := filemgr.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if size != int64(len(testData)) {
		t.Fatalf("size = %d, want %d", size, len(testData))
	}
	decoded, _ := base64.StdEncoding.DecodeString(readContent)
	if string(decoded) != testData {
		t.Fatalf("decoded content = %q, want %q", string(decoded), testData)
	}

	// 3. List directory
	files, err := filemgr.ListDirectory(filepath.Join(tempDir, "subfolder"))
	if err != nil {
		t.Fatalf("list directory: %v", err)
	}
	if len(files) != 1 || files[0].Name != "test.txt" || files[0].IsDir {
		t.Fatalf("unexpected files list: %+v", files)
	}

	// 4. Delete file
	if err := filemgr.DeleteFile(targetFile); err != nil {
		t.Fatalf("delete file: %v", err)
	}

	if _, err := os.Stat(targetFile); !os.IsNotExist(err) {
		t.Fatal("file was not deleted")
	}
}
