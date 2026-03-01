package tools

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/media"
)

// SendFileTool allows the agent to send a local file (image, document, etc.)
// as a media attachment to the user via the channel's SendMedia capability.
type SendFileTool struct {
	workingDir          string
	restrictToWorkspace bool
	mediaStore          media.MediaStore
	channel             string
	chatID              string
}

func NewSendFileTool(workspace string, restrict bool) *SendFileTool {
	return &SendFileTool{
		workingDir:          workspace,
		restrictToWorkspace: restrict,
	}
}

func (t *SendFileTool) Name() string {
	return "send_file"
}

func (t *SendFileTool) Description() string {
	return "Send a local file (image, document, audio, video) to the user as a media attachment. Use this instead of reading binary files with read_file."
}

func (t *SendFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute or relative path to the file to send",
			},
		},
		"required": []string{"path"},
	}
}

// SetContext implements ContextualTool to receive per-message channel/chatID.
func (t *SendFileTool) SetContext(channel, chatID string) {
	t.channel = channel
	t.chatID = chatID
}

// SetMediaStore injects the media store for file registration.
func (t *SendFileTool) SetMediaStore(store media.MediaStore) {
	t.mediaStore = store
}

func (t *SendFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return ErrorResult("path is required")
	}

	// Resolve relative paths
	if !filepath.IsAbs(path) && t.workingDir != "" {
		path = filepath.Join(t.workingDir, path)
	}

	// Validate path exists
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrorResult(fmt.Sprintf("file not found: %s", path))
		}
		return ErrorResult(fmt.Sprintf("cannot access file: %v", err))
	}
	if info.IsDir() {
		return ErrorResult("path is a directory, not a file")
	}

	// Check workspace restriction
	if t.restrictToWorkspace && t.workingDir != "" {
		if !isWithinWorkspace(path, t.workingDir) {
			return ErrorResult("file is outside the workspace")
		}
	}

	if t.mediaStore == nil {
		return ErrorResult("media store not available — this channel may not support file sending")
	}

	filename := filepath.Base(path)
	contentType := inferContentType(filename)

	// Build a scope from current channel context
	scope := fmt.Sprintf("send_file:%s:%s:%d", t.channel, t.chatID, time.Now().UnixNano())

	ref, err := t.mediaStore.Store(path, media.MediaMeta{
		Filename:    filename,
		ContentType: contentType,
		Source:      "tool:send_file",
	}, scope)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to register file: %v", err))
	}

	return &ToolResult{
		ForLLM: fmt.Sprintf("File '%s' sent to user successfully", filename),
		Silent: false,
		Media:  []string{ref},
	}
}

// inferContentType guesses the MIME type from a filename extension.
func inferContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg":
		return "audio/ogg"
	default:
		return "application/octet-stream"
	}
}
