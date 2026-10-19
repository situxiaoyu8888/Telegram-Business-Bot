package admin

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type uploadRule struct {
	Extensions map[string]bool
	MIMETypes  map[string]bool
	MaxBytes   int64
	SubDir     string
}

func (h *Handler) uploadMedia(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file required"})
		return
	}

	mediaType := strings.ToLower(strings.TrimSpace(c.PostForm("type")))
	rule, err := h.uploadRule(mediaType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ext := strings.ToLower(filepath.Ext(file.Filename))
	if !rule.Extensions[ext] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported file extension: " + ext})
		return
	}

	if file.Size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty file"})
		return
	}

	if file.Size > rule.MaxBytes {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("file too large, max %d MB", rule.MaxBytes/1024/1024),
		})
		return
	}

	mimeType, err := detectMultipartMIME(file)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "detect file type failed: " + err.Error()})
		return
	}

	if !rule.MIMETypes[mimeType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported MIME type: " + mimeType})
		return
	}

	month := time.Now().Format("200601")
	dir := filepath.Join(h.uploadDir, rule.SubDir, month)
	if err := os.MkdirAll(dir, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create upload dir failed: " + err.Error()})
		return
	}

	randomName, err := secureRandomName(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create filename failed"})
		return
	}

	filename := randomName + ext
	dst := filepath.Join(dir, filename)

	if err := c.SaveUploadedFile(file, dst); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save upload failed: " + err.Error()})
		return
	}

	relPath := filepath.ToSlash(filepath.Join(rule.SubDir, month, filename))
	publicURL := h.uploadPublicBaseURL + "/" + relPath

	c.JSON(http.StatusOK, gin.H{
		"ok":            true,
		"type":          mediaType,
		"original_name": file.Filename,
		"size":          file.Size,
		"mime_type":     mimeType,
		"path":          relPath,
		"url":           publicURL,
	})
}

func (h *Handler) uploadRule(mediaType string) (uploadRule, error) {
	switch mediaType {
	case "image":
		return uploadRule{
			Extensions: map[string]bool{
				".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
			},
			MIMETypes: map[string]bool{
				"image/jpeg": true,
				"image/png":  true,
				"image/webp": true,
			},
			MaxBytes: h.uploadMaxImageMB * 1024 * 1024,
			SubDir:   "images",
		}, nil

	case "video":
		return uploadRule{
			Extensions: map[string]bool{
				".mp4": true, ".mov": true, ".m4v": true,
			},
			MIMETypes: map[string]bool{
				"video/mp4":       true,
				"video/quicktime": true,
				"video/x-m4v":     true,
			},
			MaxBytes: h.uploadMaxVideoMB * 1024 * 1024,
			SubDir:   "videos",
		}, nil

	default:
		return uploadRule{}, fmt.Errorf("type must be image or video")
	}
}

func detectMultipartMIME(file *multipart.FileHeader) (string, error) {
	f, err := file.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", err
	}

	return http.DetectContentType(buf[:n]), nil
}

func secureRandomName(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
