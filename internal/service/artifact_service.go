package service

// ArtifactService 制品下载（docs/基础功能边界与插件化可行性评估.md R4）。
//
// 制品 = 全局快照 CAS 内容（data/snapshots/blobs/<xx>/<sha256>），id 即 sha256 十六进制串。
// 安全性核心：id 走**严格白名单**（^[a-f0-9]{64}$），任何其他形态——含 "../" 路径穿越、
// 路径分隔符、空串、超长串——一律 400。CAS 的落盘路径只由 hash 前两位拼分桶，
// 白名单校验后不存在任何用户可控的路径片段，因此不存在穿越面。

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"harness-go/internal/apperr"
	"harness-go/internal/snapshot"
)

// artifactProbeBytes 内容嗅探字节数（http.DetectContentType 的推荐上限）。
const artifactProbeBytes = 512

// artifactIDPattern 制品 id 白名单：64 位小写 sha256 十六进制。
var artifactIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ArtifactMeta 制品元信息（下载响应头依据）。
type ArtifactMeta struct {
	ID          string
	Size        int64
	ContentType string
	FileName    string
}

// ArtifactServiceOptions 制品服务依赖。
type ArtifactServiceOptions struct {
	// Blobs 全局快照 CAS；为 nil 时按 DataDir 惰性构造。
	Blobs *snapshot.GlobalBlobs
	// DataDir 数据目录（快照 CAS 挂 data/snapshots/blobs）。
	DataDir string
}

// ArtifactService 制品下载。
type ArtifactService struct {
	blobs   *snapshot.GlobalBlobs
	initErr error
}

// NewArtifactService 创建制品服务（CAS 初始化失败时保留错误，端点返回 internal）。
func NewArtifactService(opts ArtifactServiceOptions) *ArtifactService {
	s := &ArtifactService{blobs: opts.Blobs}
	if s.blobs == nil && strings.TrimSpace(opts.DataDir) != "" {
		blobs, err := snapshot.NewGlobalBlobs(opts.DataDir)
		s.blobs, s.initErr = blobs, err
	}
	return s
}

// Open 读取制品内容（调用方负责 Close）。
//
// 错误码约定：
//   - id 非 64 位 sha256 十六进制 → invalid_argument(400)
//   - CAS 无此内容 → not_found(404)
//   - 子系统未接线 / CAS 初始化失败 → unavailable(503) / internal(500)
func (s *ArtifactService) Open(ctx context.Context, id string) (io.ReadCloser, ArtifactMeta, error) {
	var meta ArtifactMeta
	if s == nil || s.blobs == nil {
		if s != nil && s.initErr != nil {
			return nil, meta, apperr.Wrap(s.initErr, apperr.CodeInternal, "快照 CAS 初始化失败")
		}
		return nil, meta, apperr.New(apperr.CodeUnavailable, "制品下载子系统未接线（快照 CAS 缺失）")
	}
	hash := strings.ToLower(strings.TrimSpace(id))
	if !artifactIDPattern.MatchString(hash) {
		return nil, meta, apperr.New(apperr.CodeInvalidArgument,
			"制品 id 非法：必须为 64 位 sha256 十六进制串")
	}
	rc, err := s.blobs.Open(ctx, hash)
	if err != nil {
		if errors.Is(err, snapshot.ErrInvalidSnapshot) {
			return nil, meta, apperr.Wrap(err, apperr.CodeNotFound, "制品不存在：快照 CAS 无此内容")
		}
		return nil, meta, apperr.Wrap(err, apperr.CodeInternal, "读取制品失败")
	}
	size := int64(0)
	if fi, statErr := os.Stat(s.blobs.Path(hash)); statErr == nil {
		size = fi.Size()
	}
	br := bufio.NewReaderSize(rc, artifactProbeBytes*2)
	head, _ := br.Peek(artifactProbeBytes) // 短内容返回 EOF，head 已足够嗅探
	ct := http.DetectContentType(head)
	meta = ArtifactMeta{
		ID:          hash,
		Size:        size,
		ContentType: ct,
		FileName:    artifactFileName(hash, ct),
	}
	return &artifactReader{Reader: br, Closer: rc}, meta, nil
}

// artifactReader 嗅探后仍需保留原始 Closer。
type artifactReader struct {
	*bufio.Reader
	io.Closer
}

// artifactFileName 生成下载文件名（hash 短标识 + 按内容类型推断扩展名）。
func artifactFileName(hash, contentType string) string {
	short := hash
	if len(short) > 16 {
		short = short[:16]
	}
	return "artifact-" + short + artifactExt(contentType)
}

func artifactExt(contentType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/png"):
		return ".png"
	case strings.HasPrefix(contentType, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(contentType, "image/gif"):
		return ".gif"
	case strings.HasPrefix(contentType, "image/webp"):
		return ".webp"
	case strings.HasPrefix(contentType, "image/svg"):
		return ".svg"
	case strings.HasPrefix(contentType, "application/pdf"):
		return ".pdf"
	case strings.HasPrefix(contentType, "application/zip"):
		return ".zip"
	case strings.HasPrefix(contentType, "application/json"):
		return ".json"
	case strings.HasPrefix(contentType, "text/plain"):
		return ".txt"
	case strings.HasPrefix(contentType, "text/csv"):
		return ".csv"
	case strings.HasPrefix(contentType, "text/html"):
		return ".html"
	default:
		return ".bin"
	}
}
