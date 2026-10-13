// Package files 在环境侧本地执行文件操作（读/写/列目录/元信息）。
// M2：整文件传输（base64），单文件上限 20MB；分块流式留待后续里程碑。
package files

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"envpilot/internal/proto"
)

// 单文件大小上限，超过则拒绝（防止打爆 WS 帧/网关内存）
const maxFileSize = 20 << 20 // 20MB

// Read 读取文件内容（base64 编码返回，二进制安全）。
func Read(req proto.FileReadRequest) proto.FileReadResult {
	max := req.MaxBytes
	if max <= 0 || max > maxFileSize {
		max = maxFileSize
	}

	st, err := os.Stat(req.Path)
	if err != nil {
		return proto.FileReadResult{Error: fmt.Sprintf("stat: %v", err)}
	}
	if st.IsDir() {
		return proto.FileReadResult{Error: "path is a directory"}
	}
	if st.Size() > max {
		return proto.FileReadResult{
			Size:  st.Size(),
			Error: fmt.Sprintf("file is %d bytes, exceeds limit %d bytes", st.Size(), max),
		}
	}

	data, err := os.ReadFile(req.Path)
	if err != nil {
		return proto.FileReadResult{Error: fmt.Sprintf("read: %v", err)}
	}
	return proto.FileReadResult{
		ContentBase64: base64.StdEncoding.EncodeToString(data),
		Size:          st.Size(),
	}
}

// Write 写入文件（覆盖或追加），自动创建父目录。
func Write(req proto.FileWriteRequest) proto.FileWriteResult {
	data, err := base64.StdEncoding.DecodeString(req.ContentBase64)
	if err != nil {
		return proto.FileWriteResult{Error: fmt.Sprintf("invalid base64 content: %v", err)}
	}
	if int64(len(data)) > maxFileSize {
		return proto.FileWriteResult{Error: fmt.Sprintf("content is %d bytes, exceeds limit %d bytes", len(data), maxFileSize)}
	}

	if dir := filepath.Dir(req.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return proto.FileWriteResult{Error: fmt.Sprintf("mkdir: %v", err)}
		}
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if req.Append {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(req.Path, flags, 0o644)
	if err != nil {
		return proto.FileWriteResult{Error: fmt.Sprintf("open: %v", err)}
	}
	defer f.Close()

	n, err := f.Write(data)
	if err != nil {
		return proto.FileWriteResult{BytesWritten: int64(n), Error: fmt.Sprintf("write: %v", err)}
	}
	return proto.FileWriteResult{BytesWritten: int64(n)}
}

// ListDir 列出目录条目（按名称排序）。
func ListDir(req proto.ListDirRequest) proto.ListDirResult {
	entries, err := os.ReadDir(req.Path)
	if err != nil {
		return proto.ListDirResult{Error: fmt.Sprintf("readdir: %v", err)}
	}

	out := make([]proto.DirEntry, 0, len(entries))
	for _, e := range entries {
		entry := proto.DirEntry{Name: e.Name(), IsDir: e.IsDir()}
		if info, err := e.Info(); err == nil {
			entry.Size = info.Size()
			entry.ModTime = info.ModTime()
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return proto.ListDirResult{Entries: out}
}

// Stat 返回文件元信息（不存在时 Exists=false，而非错误）。
func Stat(req proto.FileStatRequest) proto.FileStatResult {
	st, err := os.Stat(req.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return proto.FileStatResult{}
		}
		return proto.FileStatResult{Error: fmt.Sprintf("stat: %v", err)}
	}
	return proto.FileStatResult{
		Exists:  true,
		Size:    st.Size(),
		IsDir:   st.IsDir(),
		Mode:    st.Mode().String(),
		ModTime: st.ModTime(),
	}
}
