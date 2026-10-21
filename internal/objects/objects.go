package objects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/MoeclubM/metafusion-storage/internal/config"
)

// Part 是客户端直传完成后回传的分片信息（ETag 取自 PUT 响应头）。
type Part struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

// Store 封装对象存储：内部端点负责服务端读写与分片合并，对外端点只用于签发直传地址。
// 预签名的 SigV4 覆盖 Host，因此对外地址必须单独配置，不能用内部服务名签发。
// 未配置 S3 端点时回落到本地对象模式，保证没有 RustFS 也能开发与自测。
type Store struct {
	root   string
	bucket string
	local  bool
	ttl    time.Duration
	// verifyMaxBytes / verifyTimeout 是 complete 阶段回读重算摘要的上限（0 为不限制）。
	// 放在 Store 上而不是逐个调用点传参：上限是"这类对象存储能承受多大的整份回读"的
	// 部署属性，不是单次请求的属性。
	verifyMaxBytes int64
	verifyTimeout  time.Duration
	client         *minio.Client
	core           *minio.Core
	signer         *minio.Client
}

func New(ctx context.Context, cfg config.Config) (*Store, error) {
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		root:           cfg.Root,
		bucket:         cfg.S3Bucket,
		ttl:            cfg.PresignTTL,
		verifyMaxBytes: int64(cfg.VerifyMaxMB) << 20,
		verifyTimeout:  cfg.VerifyTimeout,
	}
	if cfg.S3Endpoint == "" {
		// 端点为空即本地对象模式是隐式的：凭据配了而端点没配（键名拼错、漏注入、
		// 编排改了变量名）会静默落到另一套存储上。两套存储都能启动成功，现象要等到
		// 读旧对象时才出现，而且和"对象存储故障"长得一样。宁可在这里启动失败。
		if cfg.S3AccessKey != "" || cfg.S3SecretKey != "" {
			return nil, errors.New("STORAGE_S3_ENDPOINT 为空但已配置对象存储凭据：对象模式不明确，拒绝以本地模式启动")
		}
		s.local = true
		return s, nil
	}
	opts := &minio.Options{Creds: credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""), Secure: cfg.S3TLS}
	client, err := minio.New(cfg.S3Endpoint, opts)
	if err != nil {
		return nil, err
	}
	core, err := minio.NewCore(cfg.S3Endpoint, opts)
	if err != nil {
		return nil, err
	}
	var signer *minio.Client
	if cfg.S3PublicEndpoint != "" {
		signer, err = minio.New(cfg.S3PublicEndpoint, opts)
		if err != nil {
			return nil, err
		}
	}
	// 最小权限：STORAGE_S3_SKIP_BUCKET_ENSURE=true 时跳过建桶/查桶，只用仅目标桶数据操作
	// 的服务凭据启动（s3:GetObject/PutObject/DeleteObject/AbortMultipart 等，不含
	// s3:CreateBucket）。桶由运维身份事先建好；跳过的是"保证桶存在"，不是"保证能读写"——
	// 读写失败仍在首次请求时显式报错，不会静默。
	if !cfg.S3SkipBucketEnsure {
		probe, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err = ensureBucket(probe, client, cfg.S3Bucket); err != nil {
			return nil, err
		}
	} else if cfg.S3Bucket == "" {
		return nil, errors.New("STORAGE_S3_BUCKET 为空：跳过建桶时必须显式指定目标桶")
	}
	s.client, s.core, s.signer = client, core, signer
	return s, nil
}

// ensureBucket 保证本服务的桶存在，桶由拥有它的服务自己创建。
//
// 原先交给一次性初始化容器（minio/mc 镜像）创建，代价是：多一个外部镜像依赖
// （该镜像已从 Docker Hub 撤下，拉不到就整条部署链失败），多一类"初始化容器没
// 跑完就启动"的顺序故障，而存储服务本身已经连着对象存储，判定条件完全一致。
// 先查后建；并发下两个实例同时建时按"已存在"容忍，因此本函数幂等。
func ensureBucket(ctx context.Context, client *minio.Client, bucket string) error {
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("object store unavailable: %w", err)
	}
	if exists {
		return nil
	}
	if err = client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		switch minio.ToErrorResponse(err).Code {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return nil
		}
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	return nil
}

// Local 表示当前是本地对象模式：没有预签名，直传走服务端流式接收端点。
func (s *Store) Local() bool { return s.local }

// DirectTransfer requires an explicitly configured public signing endpoint.
// Internal-only S3 still stores objects remotely; HTTP transfers go through this service.
func (s *Store) DirectTransfer() bool { return s.signer != nil }

// KeyFor 生成内容寻址的对象键：同一 sha256 恒定映射同一键。
// 文件名只作为可读后缀，不参与身份，避免同名不同内容互相覆盖。
func (s *Store) KeyFor(sha256hex, name string) string {
	prefix := sha256hex
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	return "objects/" + prefix + "/" + sha256hex + "/" + sanitizeName(name)
}

func sanitizeName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	// ".." 会被 filepath.Base 原样返回，点号又在允许字符集内：不拦住它，
	// 对象键就会多出一层父目录引用（<sha>/..），本地模式落盘时变成对着目录 rename 而失败。
	if base == "" || base == "." || base == ".." || base == "/" || base == "\\" {
		return "blob"
	}
	out := make([]rune, 0, len(base))
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case strings.ContainsRune("._- ()[]{}@+", r):
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// NewParts 建立一个分片上传会话；续传时复用已有的 uploadID，不重复建会话。
func (s *Store) NewParts(ctx context.Context, key, mime string, partCount int) (string, error) {
	if s.local || partCount <= 1 {
		return "", nil
	}
	return s.core.NewMultipartUpload(ctx, s.bucket, key, minio.PutObjectOptions{ContentType: mime})
}

// PresignParts 签发直传地址：uploadID 为空表示单次 PUT，否则逐片签发 UploadPart 地址。
func (s *Store) PresignParts(ctx context.Context, key, mime string, partCount int, uploadID string) ([]string, error) {
	if !s.DirectTransfer() {
		return nil, nil
	}
	if uploadID == "" {
		u, err := s.signer.PresignedPutObject(ctx, s.bucket, key, s.ttl)
		if err != nil {
			return nil, err
		}
		return []string{u.String()}, nil
	}
	urls := make([]string, 0, partCount)
	for i := 1; i <= partCount; i++ {
		u, err := s.signer.Presign(ctx, http.MethodPut, s.bucket, key, s.ttl, url.Values{
			"uploadId":   {uploadID},
			"partNumber": {strconv.Itoa(i)},
		})
		if err != nil {
			return nil, err
		}
		urls = append(urls, u.String())
	}
	return urls, nil
}

// CompleteUpload 合并分片并返回对象的**实际大小**。
//
// 大小一律用 HEAD 回读得到，不用 CompleteMultipartUpload 的返回值：
// S3 的合并响应体里没有对象长度，客户端库在该路径上返回的 Size 是 0，
// 直接采信会让"声明大小 vs 实际大小"的校验永远不通过（上传被误判为 size_mismatch）。
func (s *Store) CompleteUpload(ctx context.Context, key, uploadID string, parts []Part) (int64, error) {
	if s.local {
		return 0, nil
	}
	if uploadID != "" && len(parts) > 0 {
		complete := make([]minio.CompletePart, 0, len(parts))
		for _, p := range parts {
			complete = append(complete, minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag})
		}
		if _, err := s.core.CompleteMultipartUpload(ctx, s.bucket, key, uploadID, complete, minio.PutObjectOptions{}); err != nil {
			return 0, err
		}
	}
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

// AbortUpload 放弃分片会话，避免对象存储里堆积未完成的碎片。
func (s *Store) AbortUpload(ctx context.Context, key, uploadID string) error {
	if s.local || uploadID == "" {
		return nil
	}
	return s.core.AbortMultipartUpload(ctx, s.bucket, key, uploadID)
}

// PresignDownload 签发下载地址：文件名通过 response-content-disposition 带出，
// 避免为了改文件名而在服务端中转整份数据。
//
// 撤销窗口：地址在 ttl 内持续有效，事后改数据库状态撤销不了已签发的地址
// （见 download 的注释）。立即禁发需求用短期签名或走鉴权代理的 content 口。
func (s *Store) PresignDownload(ctx context.Context, key, fileName string) (string, time.Time, error) {
	expires := time.Now().Add(s.ttl)
	if !s.DirectTransfer() {
		return "", expires, nil
	}
	disposition := "attachment"
	if fileName != "" {
		disposition = mimeDisposition(fileName)
	}
	u, err := s.signer.PresignedGetObject(ctx, s.bucket, key, s.ttl, url.Values{
		"response-content-disposition": {disposition},
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return u.String(), expires, nil
}

func mimeDisposition(name string) string {
	// sanitizeName 已把引号等不安全字符替换为下划线，这里只负责拼接 disposition。
	return "attachment; filename=\"" + sanitizeName(name) + "\""
}

// ErrHashMismatch 表示请求体的实际 sha256 与调用方声明的值不符。
//
// 校验发生在**发布对象之前**（临时文件阶段），因此内容寻址键上永远不会留下
// "键是 X、内容是 Y" 的对象：声明不符时对象根本不进对象存储，临时文件随 defer 删除。
// 这一点是内容寻址的前提——先发布再比对，比对失败后对象已经躺在键 X 上了。
var ErrHashMismatch = errors.New("hash_mismatch")

// PutStream 把请求体流式写入对象存储并同时计算 sha256：不把整份文件读进内存。
// expectedSha256 非空时必须与实际摘要一致，否则返回 ErrHashMismatch 且不写入对象。
func (s *Store) PutStream(ctx context.Context, key string, r io.Reader, expectedSha256 string) (int64, string, error) {
	tmp, err := os.CreateTemp(s.root, "incoming-")
	if err != nil {
		return 0, "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hash), r)
	closeErr := tmp.Close()
	if err != nil {
		return 0, "", err
	}
	if closeErr != nil {
		return 0, "", closeErr
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if expectedSha256 != "" && digest != expectedSha256 {
		return size, digest, ErrHashMismatch
	}
	if s.local {
		dest := filepath.Join(s.root, filepath.FromSlash(key))
		if err = os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return 0, "", err
		}
		if err = os.Rename(tmpName, dest); err != nil {
			return 0, "", err
		}
		return size, digest, nil
	}
	if _, err = s.client.FPutObject(ctx, s.bucket, key, tmpName, minio.PutObjectOptions{}); err != nil {
		return 0, "", err
	}
	return size, digest, nil
}

// Remove 删除对象，用于清掉内容寻址键上的"校验没通过"的对象：
// 键是 sha256，键上留着与声明不符的内容，就等于把这个 sha256 的位置一直占着
// （本服务不签发带条件头的直传地址，无法在发布前拦住客户端写什么）。
func (s *Store) Remove(ctx context.Context, key string) error {
	if s.local {
		err := os.Remove(filepath.Join(s.root, filepath.FromSlash(key)))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// Open 打开对象内容，用于服务端校验哈希与流式下载。
func (s *Store) Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	if s.local {
		f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(key)))
		if err != nil {
			return nil, 0, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, 0, err
		}
		return f, info.Size(), nil
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, 0, err
	}
	return obj, info.Size, nil
}

// IsObjectMissing 报告 err 是否表示"这个键上没有对象"，而不是对象存储不可用。
//
// 两者对外必须是两个结论：键上没有对象是数据缺失，重试、扩容、换端点都不会让它出现；
// 对象存储/网络/凭据故障才是"暂时不可用"（503）。此前读失败一律按 storage_unavailable
// 返回，于是"对象早就没了"与"对象存储挂了"在响应与告警里完全同形。
func IsObjectMissing(err error) bool {
	if err == nil {
		return false
	}
	// 本地对象模式：键上没文件就是没这个对象。
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	// 对象存储侧只认"这个键不存在"。桶不存在（NoSuchBucket）与签名/权限错误
	// 都是部署问题，不是数据缺失：它们按 503 报，才不会被当成"内容已丢"处理。
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.Code == "NoSuchObject"
	}
	return false
}

// HashOf 读回对象并计算 sha256（不带声明比对与上限），保留给"只看内容摘要"的调用方。
func (s *Store) HashOf(ctx context.Context, key string) (string, int64, error) {
	return s.VerifyHash(ctx, key, "")
}
