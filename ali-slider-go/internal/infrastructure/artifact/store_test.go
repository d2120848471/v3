package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixtureEntropy struct {
	mu   sync.Mutex
	next uint64
}

func (f *fixtureEntropy) Read(buffer []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.next
	f.next++
	for index := range buffer {
		buffer[index] = byte(value >> (uint(index%8) * 8))
	}
	return len(buffer), nil
}

func (f *fixtureEntropy) Uint64n(limit uint64) (uint64, error) { return 0, nil }

func TestSaveFailureIsSanitizedAndPrivate(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "artifacts")
	now := time.Date(2026, 8, 7, 1, 2, 3, 0, time.UTC)
	store := Store{Directory: directory, Retention: 7 * 24 * time.Hour, Entropy: &fixtureEntropy{}, Now: func() time.Time { return now }}
	identifier, err := store.SaveFailure([]byte("back"), []byte("shadow"), Metrics{Reason: "low-confidence", Stage: "vision", Confidence: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if len(identifier) != 32 {
		t.Fatalf("invalid id %q", identifier)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected three files, got %d", len(entries))
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode=%v", directoryInfo.Mode().Perm())
	}
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact %q mode=%v err=%v", entry.Name(), info.Mode().Perm(), infoErr)
		}
	}
	metrics, err := os.ReadFile(filepath.Join(directory, identifier+"-metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(metrics)
	for _, forbidden := range []string{"securityToken", "certifyId", "rawResponse"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metrics leaked %s: %s", forbidden, text)
		}
	}
}

func TestSaveFailureSetQuotaEvictsOldestCompleteGroup(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "artifacts")
	now := time.Now()
	store := Store{
		Directory: directory, Retention: 24 * time.Hour, Entropy: &fixtureEntropy{}, Now: func() time.Time { return now },
		MaxTotalBytes: 1 << 20, MaxSets: 2,
	}
	first := saveThreeFileSet(t, store)
	setGroupTime(t, directory, first, now.Add(-3*time.Hour))
	second := saveThreeFileSet(t, store)
	setGroupTime(t, directory, second, now.Add(-2*time.Hour))
	unmanaged := filepath.Join(directory, "keep.txt")
	if err := os.WriteFile(unmanaged, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	managedLooking := filepath.Join(directory, strings.Repeat("e", 32)+"-back.json")
	if err := os.WriteFile(managedLooking, []byte("not-managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	third := saveThreeFileSet(t, store)

	if files := filesForGroup(t, directory, first); len(files) != 0 {
		t.Fatalf("oldest group was only partially evicted: %v", files)
	}
	for _, identifier := range []string{second, third} {
		if files := filesForGroup(t, directory, identifier); len(files) != 3 {
			t.Fatalf("group %s files=%v", identifier, files)
		}
	}
	if content, err := os.ReadFile(unmanaged); err != nil || string(content) != "keep" {
		t.Fatalf("unmanaged file changed: content=%q err=%v", content, err)
	}
	if content, err := os.ReadFile(managedLooking); err != nil || string(content) != "not-managed" {
		t.Fatalf("managed-looking file changed: content=%q err=%v", content, err)
	}
}

func TestSaveFailureByteQuotaEvictsOldestGroup(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "artifacts")
	now := time.Now()
	store := Store{
		Directory: directory, Retention: 24 * time.Hour, Entropy: &fixtureEntropy{}, Now: func() time.Time { return now },
		MaxTotalBytes: 1 << 20, MaxSets: 10,
	}
	first := saveThreeFileSet(t, store)
	setGroupTime(t, directory, first, now.Add(-3*time.Hour))
	setBytes := groupBytes(t, directory, first)
	second := saveThreeFileSet(t, store)
	setGroupTime(t, directory, second, now.Add(-2*time.Hour))
	if got := groupBytes(t, directory, second); got != setBytes {
		t.Fatalf("fixture group sizes differ: first=%d second=%d", setBytes, got)
	}
	store.MaxTotalBytes = 2 * setBytes
	third := saveThreeFileSet(t, store)
	if len(filesForGroup(t, directory, first)) != 0 {
		t.Fatal("byte quota did not evict the oldest complete group")
	}
	if groupBytes(t, directory, second)+groupBytes(t, directory, third) > store.MaxTotalBytes {
		t.Fatal("managed files exceed byte quota")
	}
}

func TestSaveFailureRejectsOversizedSetWithoutFiles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "artifacts")
	store := Store{
		Directory: directory, Retention: time.Hour, Entropy: &fixtureEntropy{},
		MaxTotalBytes: 1, MaxSets: 1,
	}
	if _, err := store.SaveFailure([]byte("back"), []byte("shadow"), Metrics{Reason: "fixture", Stage: "test"}); err == nil {
		t.Fatal("oversized artifact set was accepted")
	}
	if entries, err := os.ReadDir(directory); !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
		t.Fatalf("oversized set left files: entries=%v err=%v", entries, err)
	}
}

func TestSaveFailureRollsBackPartialGroup(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	identifier := strings.Repeat("0", 32)
	existing := filepath.Join(directory, identifier+"-shadow.png")
	if err := os.WriteFile(existing, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Directory: directory, Retention: time.Hour, Entropy: &fixtureEntropy{}}
	if _, err := store.SaveFailure([]byte("new-back"), []byte("new-shadow"), Metrics{Reason: "fixture", Stage: "test"}); err == nil {
		t.Fatal("expected exclusive-create collision")
	}
	if _, err := os.Stat(filepath.Join(directory, identifier+"-back.png")); !os.IsNotExist(err) {
		t.Fatalf("partial back file survived rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, identifier+"-metrics.json")); !os.IsNotExist(err) {
		t.Fatalf("partial metrics file survived rollback: %v", err)
	}
	if content, err := os.ReadFile(existing); err != nil || string(content) != "existing" {
		t.Fatalf("preexisting file changed: content=%q err=%v", content, err)
	}
}

func TestPurgeUsesWholeGroupsAndProtectsUnmanagedAndSymlinks(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "artifacts")
	now := time.Now()
	store := Store{Directory: directory, Retention: time.Hour, Entropy: &fixtureEntropy{}, Now: func() time.Time { return now }}
	identifier := saveThreeFileSet(t, store)
	files := filesForGroup(t, directory, identifier)
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(directory, files[0]), old, old); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.Purge(); err != nil || removed != 0 {
		t.Fatalf("partly old group purged: removed=%d err=%v", removed, err)
	}
	setGroupTime(t, directory, identifier, old)

	unmanaged := filepath.Join(directory, "keep.txt")
	if err := os.WriteFile(unmanaged, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.png")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, strings.Repeat("f", 32)+"-back.png")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	managedDirectory := filepath.Join(directory, strings.Repeat("d", 32)+"-metrics.json")
	if err := os.Mkdir(managedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Purge()
	if err != nil || removed != 3 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if len(filesForGroup(t, directory, identifier)) != 0 {
		t.Fatal("expired group was only partially removed")
	}
	if content, err := os.ReadFile(unmanaged); err != nil || string(content) != "keep" {
		t.Fatalf("unmanaged file changed: content=%q err=%v", content, err)
	}
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("managed-looking symlink changed: info=%v err=%v", info, err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "target" {
		t.Fatalf("symlink target changed: content=%q err=%v", content, err)
	}
	if info, err := os.Stat(managedDirectory); err != nil || !info.IsDir() {
		t.Fatalf("managed-looking directory changed: info=%v err=%v", info, err)
	}
}

func TestPurgeEnforcesLoweredSetQuota(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "artifacts")
	now := time.Now()
	store := Store{
		Directory: directory, Retention: 24 * time.Hour, Entropy: &fixtureEntropy{}, Now: func() time.Time { return now },
		MaxTotalBytes: 1 << 20, MaxSets: 3,
	}
	first := saveThreeFileSet(t, store)
	setGroupTime(t, directory, first, now.Add(-3*time.Hour))
	saveThreeFileSet(t, store)
	saveThreeFileSet(t, store)
	store.MaxSets = 2
	removed, err := store.Purge()
	if err != nil || removed != 3 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if len(filesForGroup(t, directory, first)) != 0 {
		t.Fatal("Purge did not evict oldest group after quota reduction")
	}
}

func TestDirectoryPermissionsAndDirectorySymlinkRejection(t *testing.T) {
	base := t.TempDir()
	directory := filepath.Join(base, "artifacts")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	store := Store{Directory: directory, Retention: time.Hour, Entropy: &fixtureEntropy{}}
	if _, err := store.SaveFailure(nil, nil, Metrics{Reason: "fixture", Stage: "test"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions=%v", info.Mode().Perm())
	}

	link := filepath.Join(base, "artifact-link")
	if err := os.Symlink(directory, link); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	linked := Store{Directory: link, Retention: time.Hour, Entropy: &fixtureEntropy{}}
	if _, err := linked.SaveFailure(nil, nil, Metrics{Reason: "fixture", Stage: "test"}); err == nil {
		t.Fatal("symlink artifact directory was accepted")
	}
	if _, err := linked.Purge(); err == nil {
		t.Fatal("Purge accepted a symlink artifact directory")
	}
}

func TestFilesystemRootIsNeverRepermissioned(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(os.PathSeparator)
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Directory: root, Retention: time.Hour, Entropy: &fixtureEntropy{}}
	if _, err := store.SaveFailure(nil, nil, Metrics{Reason: "fixture", Stage: "test"}); err == nil {
		t.Fatal("filesystem root was accepted as artifact directory")
	}
	after, err := os.Stat(root)
	if err != nil || before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("filesystem root permissions changed: before=%v after=%v err=%v", before.Mode().Perm(), after.Mode().Perm(), err)
	}
}

func TestConcurrentSaveAndPurgeRespectHardQuotas(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "artifacts")
	store := Store{
		Directory: directory, Retention: 24 * time.Hour, Entropy: &fixtureEntropy{},
		MaxTotalBytes: 1 << 20, MaxSets: 8,
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 80)
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.SaveFailure([]byte("back"), []byte("shadow"), Metrics{Reason: "fixture", Stage: "test"})
			errorsSeen <- err
		}()
	}
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Purge()
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	groups, err := scanManagedGroups(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) > store.MaxSets {
		t.Fatalf("groups=%d quota=%d", len(groups), store.MaxSets)
	}
	total := int64(0)
	for _, group := range groups {
		if len(group.files) != 3 {
			t.Fatalf("partial group %s has %d files", group.id, len(group.files))
		}
		total += group.bytes
	}
	if total > store.MaxTotalBytes {
		t.Fatalf("bytes=%d quota=%d", total, store.MaxTotalBytes)
	}
}

func TestStoreValidationMissingPurgeAndCleanupErrors(t *testing.T) {
	if _, err := (Store{}).Purge(); err == nil {
		t.Fatal("invalid zero-value store was accepted")
	}
	missing := Store{Directory: filepath.Join(t.TempDir(), "missing"), Retention: time.Hour, Entropy: &fixtureEntropy{}}
	if removed, err := missing.Purge(); err != nil || removed != 0 {
		t.Fatalf("missing Purge removed=%d err=%v", removed, err)
	}
	invalidQuota := missing
	invalidQuota.MaxSets = -1
	if _, err := invalidQuota.SaveFailure(nil, nil, Metrics{}); err == nil {
		t.Fatal("negative quota was accepted")
	}
	if wrapCleanupError("fixture", nil) != nil || wrapCleanupError("fixture", os.ErrNotExist) != nil {
		t.Fatal("harmless cleanup result became an error")
	}
	if err := wrapCleanupError("fixture", errors.New("failure")); err == nil || !strings.Contains(err.Error(), "fixture") {
		t.Fatalf("cleanup error=%v", err)
	}
}

func saveThreeFileSet(t *testing.T, store Store) string {
	t.Helper()
	identifier, err := store.SaveFailure([]byte("back"), []byte("shadow"), Metrics{Reason: "fixture", Stage: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return identifier
}

func filesForGroup(t *testing.T, directory, identifier string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0, 3)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), identifier+"-") && managedName.MatchString(entry.Name()) && entry.Type()&os.ModeSymlink == 0 {
			files = append(files, entry.Name())
		}
	}
	return files
}

func setGroupTime(t *testing.T, directory, identifier string, when time.Time) {
	t.Helper()
	for _, name := range filesForGroup(t, directory, identifier) {
		if err := os.Chtimes(filepath.Join(directory, name), when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func groupBytes(t *testing.T, directory, identifier string) int64 {
	t.Helper()
	total := int64(0)
	for _, name := range filesForGroup(t, directory, identifier) {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}
