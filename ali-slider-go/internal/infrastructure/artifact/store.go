// Package artifact 保存严格脱敏的失败/低置信样本，并清理过期文件。
package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

const (
	// DefaultMaxTotalBytes 是失败样本目录的默认硬字节上限。
	DefaultMaxTotalBytes int64 = 512 << 20
	// DefaultMaxSets 是失败样本目录的默认完整样本组上限。
	DefaultMaxSets = 64
)

var (
	managedName = regexp.MustCompile(`^([0-9a-f]{32})-(back\.png|shadow\.png|metrics\.json)$`)

	// ponytail: 失败落盘是低频路径，先用单进程全局锁保证所有目录别名下的
	// Save/Purge 都串行；只有它成为可测瓶颈时才按目录分片锁。
	storeMu sync.Mutex
)

// Metrics 只允许算法与耗时字段。不要增加 token、CertifyId、上游响应或请求正文。
type Metrics struct {
	RecordedAt string         `json:"recordedAt"`
	Reason     string         `json:"reason"`
	Stage      string         `json:"stage"`
	Confidence float64        `json:"confidence,omitempty"`
	XPos       int            `json:"xPos,omitempty"`
	SlidePos   int            `json:"slidePos,omitempty"`
	TimingsMS  map[string]int `json:"timingsMs,omitempty"`
}

// Store 是一个可并发使用的失败样本文件存储。MaxTotalBytes 和 MaxSets
// 为零时使用导出默认值；配额只统计本包管理的普通非符号链接文件。
type Store struct {
	Directory     string
	Retention     time.Duration
	Entropy       runtimekit.Entropy
	Now           func() time.Time
	MaxTotalBytes int64
	MaxSets       int
}

// SaveFailure 写入一组失败图片和 JSON 指标。空图片不会创建对应文件。
// 写入前会按完整 32 位十六进制 ID 组清理过期记录并淘汰最旧组。
func (s Store) SaveFailure(background, shadow []byte, metrics Metrics) (string, error) {
	s, err := s.normalized()
	if err != nil {
		return "", err
	}
	storeMu.Lock()
	defer storeMu.Unlock()

	now := s.now()
	metrics.RecordedAt = now.UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(metrics)
	if err != nil {
		return "", fmt.Errorf("encode artifact metrics: %w", err)
	}
	items := []artifactItem{{"back.png", background}, {"shadow.png", shadow}, {"metrics.json", encoded}}
	newBytes := int64(0)
	for _, item := range items {
		newBytes += int64(len(item.data))
	}
	if newBytes > s.MaxTotalBytes {
		return "", fmt.Errorf("artifact set size %d exceeds quota %d", newBytes, s.MaxTotalBytes)
	}

	if err := ensureDirectory(s.Directory, true); err != nil {
		return "", err
	}
	groups, err := scanManagedGroups(s.Directory)
	if err != nil {
		return "", err
	}
	if _, err := pruneGroups(s.Directory, groups, now.Add(-s.Retention), s.MaxTotalBytes, s.MaxSets, newBytes, 1); err != nil {
		return "", err
	}

	identifier, err := runtimekit.Hex(s.Entropy, 16)
	if err != nil {
		return "", fmt.Errorf("artifact id: %w", err)
	}
	created := make([]string, 0, len(items))
	rollback := func() error {
		var rollbackErrors []error
		for _, path := range created {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback artifact: %w", removeErr))
			}
		}
		return errors.Join(rollbackErrors...)
	}
	for _, item := range items {
		if len(item.data) == 0 {
			continue
		}
		path := filepath.Join(s.Directory, identifier+"-"+item.suffix)
		if err := writeExclusive(path, item.data); err != nil {
			return "", errors.Join(err, rollback())
		}
		created = append(created, path)
	}
	return identifier, nil
}

// Purge 删除过期组，并在配置收紧或外部遗留文件导致超限时淘汰最旧组。
// 它只删除名称受管、扫描后仍是同一普通文件的条目，绝不跟随或删除符号链接。
func (s Store) Purge() (int, error) {
	s, err := s.normalized()
	if err != nil {
		return 0, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	exists, err := directoryExists(s.Directory)
	if err != nil || !exists {
		return 0, err
	}
	groups, err := scanManagedGroups(s.Directory)
	if err != nil {
		return 0, err
	}
	return pruneGroups(s.Directory, groups, s.now().Add(-s.Retention), s.MaxTotalBytes, s.MaxSets, 0, 0)
}

type artifactItem struct {
	suffix string
	data   []byte
}

type managedFile struct {
	name string
	size int64
	info os.FileInfo
}

type managedGroup struct {
	id      string
	files   []managedFile
	bytes   int64
	updated time.Time
}

func scanManagedGroups(directory string) ([]managedGroup, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read artifact directory: %w", err)
	}
	byID := make(map[string]*managedGroup)
	for _, entry := range entries {
		matches := managedName.FindStringSubmatch(entry.Name())
		if len(matches) == 0 || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil, fmt.Errorf("inspect managed artifact: %w", infoErr)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		group := byID[matches[1]]
		if group == nil {
			group = &managedGroup{id: matches[1]}
			byID[matches[1]] = group
		}
		if info.Size() < 0 || group.bytes > maxInt64-info.Size() {
			return nil, errors.New("managed artifact byte count overflow")
		}
		group.files = append(group.files, managedFile{name: entry.Name(), size: info.Size(), info: info})
		group.bytes += info.Size()
		if info.ModTime().After(group.updated) {
			group.updated = info.ModTime()
		}
	}
	groups := make([]managedGroup, 0, len(byID))
	for _, group := range byID {
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].updated.Equal(groups[j].updated) {
			return groups[i].id < groups[j].id
		}
		return groups[i].updated.Before(groups[j].updated)
	})
	return groups, nil
}

func pruneGroups(directory string, groups []managedGroup, cutoff time.Time, maxBytes int64, maxSets int, extraBytes int64, extraSets int) (int, error) {
	removed := 0
	kept := groups[:0]
	totalBytes := int64(0)
	for _, group := range groups {
		if group.updated.Before(cutoff) {
			count, err := removeManagedGroup(directory, group)
			removed += count
			if err != nil {
				return removed, err
			}
			continue
		}
		if totalBytes > maxInt64-group.bytes {
			return removed, errors.New("managed artifact byte count overflow")
		}
		totalBytes += group.bytes
		kept = append(kept, group)
	}
	for len(kept)+extraSets > maxSets || exceedsByteQuota(totalBytes, extraBytes, maxBytes) {
		if len(kept) == 0 {
			return removed, errors.New("artifact quota cannot admit new set")
		}
		oldest := kept[0]
		count, err := removeManagedGroup(directory, oldest)
		removed += count
		if err != nil {
			return removed, err
		}
		totalBytes -= oldest.bytes
		kept = kept[1:]
	}
	return removed, nil
}

func removeManagedGroup(directory string, group managedGroup) (int, error) {
	for _, file := range group.files {
		current, err := os.Lstat(filepath.Join(directory, file.name))
		if err != nil {
			return 0, fmt.Errorf("recheck managed artifact: %w", err)
		}
		if !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(file.info, current) {
			return 0, errors.New("managed artifact changed during cleanup")
		}
	}
	removed := 0
	for _, file := range group.files {
		if err := os.Remove(filepath.Join(directory, file.name)); err != nil {
			return removed, fmt.Errorf("remove managed artifact: %w", err)
		}
		removed++
	}
	return removed, nil
}

func ensureDirectory(directory string, create bool) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve artifact directory: %w", err)
	}
	if filepath.Dir(absolute) == absolute {
		return errors.New("artifact directory must not be a filesystem root")
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create artifact directory: %w", err)
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return fmt.Errorf("inspect artifact directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("artifact directory must be a real directory, not a symlink")
	}
	before := info
	// Windows 的 os.Chmod 只映射 owner-write 位，FileMode 无法表达 ACL。
	// 该平台继承父目录 ACL；Unix 继续强制并复核 0700。
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("restrict artifact directory permissions: %w", err)
		}
	}
	after, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("recheck artifact directory: %w", err)
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(before, after) ||
		(runtime.GOOS != "windows" && after.Mode().Perm() != 0o700) {
		return errors.New("artifact directory security check failed")
	}
	return nil
}

func directoryExists(directory string) (bool, error) {
	if err := ensureDirectory(directory, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		var pathError *os.PathError
		if errors.As(err, &pathError) && errors.Is(pathError.Err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s Store) normalized() (Store, error) {
	if s.MaxTotalBytes == 0 {
		s.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if s.MaxSets == 0 {
		s.MaxSets = DefaultMaxSets
	}
	if s.Directory == "" || s.Retention <= 0 || s.Entropy == nil || s.MaxTotalBytes < 1 || s.MaxSets < 1 {
		return Store{}, errors.New("artifact store requires directory, positive retention, entropy and positive quotas")
	}
	return s, nil
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func exceedsByteQuota(current, additional, limit int64) bool {
	return current > limit || additional > limit-current
}

func writeExclusive(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create artifact: %w", err)
	}
	written, err := file.Write(content)
	if err == nil && written != len(content) {
		err = io.ErrShortWrite
	}
	if err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return errors.Join(
			fmt.Errorf("write artifact: %w", err),
			wrapCleanupError("close partial artifact", closeErr),
			wrapCleanupError("remove partial artifact", removeErr),
		)
	}
	if err = file.Close(); err != nil {
		return errors.Join(fmt.Errorf("close artifact: %w", err), wrapCleanupError("remove unclosed artifact", os.Remove(path)))
	}
	return nil
}

func wrapCleanupError(operation string, err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

const maxInt64 = int64(^uint64(0) >> 1)
