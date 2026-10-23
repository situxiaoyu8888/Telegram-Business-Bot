package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	// Large videos on slow links need longer than the server's read timeout.
	_ = http.NewResponseController(c.Writer).SetReadDeadline(time.Now().Add(15 * time.Minute))
	// Refuse oversized bodies while reading, before they are buffered or spilled to disk.
	limit := max(h.uploadMaxImageMB, h.uploadMaxVideoMB)*1024*1024 + 1<<20
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
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

const uploadTokenTTL = 10 * time.Minute

func uploadTokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "admin:upload:" + hex.EncodeToString(sum[:])
}

// uploadToken issues a one-time upload token for the signed-in account. Uploads go to the main
// listener, which with split listeners may not receive the console's session cookie.
func (h *Handler) uploadToken(c *gin.Context) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		respond(c, err)
		return
	}
	token := hex.EncodeToString(b)
	if err := h.sessions().Set(c.Request.Context(), uploadTokenKey(token), strconv.FormatUint(current(c).ID, 10), uploadTokenTTL).Err(); err != nil {
		c.JSON(503, gin.H{"error": "会话服务暂不可用"})
		return
	}
	url := h.uploadURL
	if url == "" {
		url = "/admin/api/upload"
	}
	c.JSON(200, gin.H{"token": token, "url": url})
}

// uploadAuth accepts an upload with a valid one-time token from an enabled account that may still
// use a page with uploads.
func (h *Handler) uploadAuth(c *gin.Context) {
	token := c.GetHeader("X-Upload-Token")
	if !validToken(token) {
		c.AbortWithStatusJSON(401, gin.H{"error": "上传凭证无效，请重试"})
		return
	}
	key := uploadTokenKey(token)
	idText, err := h.sessions().Get(c.Request.Context(), key).Result()
	if err != nil {
		c.AbortWithStatusJSON(401, gin.H{"error": "上传凭证已过期，请重试"})
		return
	}
	// Single use: only the request that actually deletes the token may upload with it.
	if n, err := h.sessions().Del(c.Request.Context(), key).Result(); err != nil || n == 0 {
		c.AbortWithStatusJSON(401, gin.H{"error": "上传凭证已使用，请重试"})
		return
	}
	id, _ := strconv.ParseUint(idText, 10, 64)
	u, err := h.accounts().ByID(c.Request.Context(), id)
	if err != nil || !u.Enabled {
		c.AbortWithStatusJSON(401, gin.H{"error": "账号不可用"})
		return
	}
	perms, err := h.permissionsOf(c.Request.Context(), u)
	if err != nil {
		c.AbortWithStatusJSON(503, gin.H{"error": "账号服务暂不可用"})
		return
	}
	allowed := false
	for _, p := range uploadPages {
		allowed = allowed || perms[p]
	}
	if !allowed {
		c.AbortWithStatusJSON(403, gin.H{"error": "没有上传权限"})
		return
	}
	c.Set(accountKey, u)
	c.Set(permissionsKey, perms)
	c.Next()
}
