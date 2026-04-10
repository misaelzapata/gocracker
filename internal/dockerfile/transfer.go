package dockerfile

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/usercfg"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/ulikunitz/xz"
)

type contextFilter struct {
	matcher *patternmatcher.PatternMatcher
}

func loadContextFilter(contextDir string) (*contextFilter, error) {
	path := filepath.Join(contextDir, ".dockerignore")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &contextFilter{}, nil
		}
		return nil, err
	}
	defer file.Close()

	patterns, err := ignorefile.ReadAll(file)
	if err != nil {
		return nil, err
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, err
	}
	return &contextFilter{matcher: matcher}, nil
}

func (f *contextFilter) ignored(rel string) (bool, error) {
	if f == nil || f.matcher == nil {
		return false, nil
	}
	rel = normalizeMatcherPath(rel)
	if rel == "." || rel == "" {
		return false, nil
	}
	return f.matcher.MatchesOrParentMatches(rel)
}

type transferOptions struct {
	allowFrom           bool
	allowRemote         bool
	autoExtractArchives bool
}

type ownershipSpec struct {
	UID int
	GID int
}

type copyOptions struct {
	preserveOwnership bool
	chown             *ownershipSpec
	chmod             *os.FileMode
	excludeMatcher    *patternmatcher.PatternMatcher
	parents           bool
	link              bool
	keepGitDir        bool
	checksum          *checksumSpec
	unpack            *bool
}

type transferSpec struct {
	srcRoot     string
	fromContext bool
	srcs        []string
	dst         string
	copyOptions copyOptions
}

type transferSource struct {
	raw       string
	abs       string
	rel       string
	info      os.FileInfo
	remoteURL *url.URL
}

type checksumSpec struct {
	algorithm string
	expected  string
}

func (s transferSource) baseName() string {
	if s.remoteURL != nil {
		base := path.Base(s.remoteURL.Path)
		if base == "." || base == "/" || base == "" {
			return "download"
		}
		return base
	}
	return filepath.Base(s.abs)
}

func (s transferSource) rootExcludePath() string {
	if s.remoteURL != nil {
		return normalizeMatcherPath(s.baseName())
	}
	rel := normalizeMatcherPath(s.rel)
	if rel == "." || rel == "" {
		return "."
	}
	return normalizeMatcherPath(filepath.Base(rel))
}

func (s transferSource) parentRelativePath() string {
	if s.remoteURL != nil {
		return normalizeMatcherPath(s.baseName())
	}
	rel := normalizeMatcherPath(s.rel)
	if rel == "." || rel == "" {
		return "."
	}
	return rel
}

func (o copyOptions) excluded(rel string) (bool, error) {
	if o.excludeMatcher == nil {
		return false, nil
	}
	rel = normalizeMatcherPath(rel)
	if rel == "" || rel == "." {
		return false, nil
	}
	return o.excludeMatcher.MatchesOrParentMatches(rel)
}

func (o copyOptions) shouldUnpack(defaultValue bool) bool {
	if o.unpack == nil {
		return defaultValue
	}
	return *o.unpack
}

func (b *builder) handleTransfer(args []string, options transferOptions) error {
	spec, err := b.parseTransferSpec(args, options)
	if err != nil {
		return err
	}
	sources, err := b.collectTransferSources(spec, options)
	if err != nil {
		return err
	}
	if spec.copyOptions.checksum != nil {
		if len(sources) != 1 || sources[0].remoteURL == nil {
			return fmt.Errorf("--checksum requires exactly one remote ADD source")
		}
	}

	dstContainerPath := b.resolveContainerPath(spec.dst)
	dstAbs := rootfsPath(b.rootfs, dstContainerPath)
	dstIsDirHint := len(sources) > 1 || strings.HasSuffix(spec.dst, "/") || spec.dst == "." || spec.dst == ".." || spec.copyOptions.parents

	for _, src := range sources {
		switch {
		case src.remoteURL != nil:
			targetPath, err := resolveTransferTarget(dstAbs, dstIsDirHint, src, spec.copyOptions)
			if err != nil {
				return err
			}
			if err := downloadToPath(src.remoteURL, targetPath, spec.copyOptions); err != nil {
				return fmt.Errorf("ADD %s → %s: %w", src.remoteURL, spec.dst, err)
			}
		case options.autoExtractArchives && spec.copyOptions.shouldUnpack(true) && spec.fromContext && src.info.Mode().IsRegular() && isLocalArchivePath(src.abs):
			if err := extractArchiveToDir(src.abs, dstAbs, spec.copyOptions); err != nil {
				return fmt.Errorf("ADD %s → %s: %w", src.raw, spec.dst, err)
			}
		case src.info.IsDir():
			copied, err := b.copyDirectorySource(src, dstAbs, spec.copyOptions, spec.fromContext)
			if err != nil {
				return fmt.Errorf("copy %s → %s: %w", src.raw, spec.dst, err)
			}
			if copied == 0 && src.rel != "." {
				if spec.fromContext {
					ignored, err := b.contextFilter.ignored(src.rel)
					if err != nil {
						return err
					}
					if ignored {
						return fmt.Errorf("source %q not found in build context or excluded by .dockerignore", src.raw)
					}
				}
				excluded, err := spec.copyOptions.excluded(src.rootExcludePath())
				if err != nil {
					return err
				}
				if excluded {
					return fmt.Errorf("source %q not found in build context or excluded by --exclude", src.raw)
				}
			}
		default:
			targetPath, err := resolveTransferTarget(dstAbs, dstIsDirHint, src, spec.copyOptions)
			if err != nil {
				return err
			}
			if err := copyPathWithOptions(src.abs, targetPath, spec.copyOptions); err != nil {
				return fmt.Errorf("copy %s → %s: %w", src.raw, spec.dst, err)
			}
		}
	}
	return nil
}

func (b *builder) parseTransferSpec(args []string, options transferOptions) (transferSpec, error) {
	spec := transferSpec{
		srcRoot:     b.opts.ContextDir,
		fromContext: true,
		copyOptions: copyOptions{},
	}
	filtered := make([]string, 0, len(args))
	excludePatterns := make([]string, 0)

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--from="):
			if !options.allowFrom {
				return transferSpec{}, fmt.Errorf("unsupported flag %q", arg)
			}
			stageRef, err := b.expandStrict(strings.TrimPrefix(arg, "--from="))
			if err != nil {
				return transferSpec{}, fmt.Errorf("expand --from: %w", err)
			}
			stage, ok := b.lookupStage(stageRef)
			if ok {
				spec.srcRoot = stage.rootfs
				spec.fromContext = false
				spec.copyOptions.preserveOwnership = true
				continue
			}
			srcRoot, err := b.resolveRemoteTransferRoot(stageRef)
			if err != nil {
				return transferSpec{}, err
			}
			spec.srcRoot = srcRoot
			spec.fromContext = false
			spec.copyOptions.preserveOwnership = true
		case strings.HasPrefix(arg, "--chown="):
			owner, err := resolveCopyOwnership(b.rootfs, b.expand(strings.TrimPrefix(arg, "--chown=")))
			if err != nil {
				return transferSpec{}, err
			}
			spec.copyOptions.chown = owner
		case strings.HasPrefix(arg, "--chmod="):
			mode, err := parseCopyMode(b.expand(strings.TrimPrefix(arg, "--chmod=")))
			if err != nil {
				return transferSpec{}, err
			}
			spec.copyOptions.chmod = mode
		case strings.HasPrefix(arg, "--exclude="):
			excludePatterns = append(excludePatterns, b.expand(strings.TrimPrefix(arg, "--exclude=")))
		case arg == "--parents":
			spec.copyOptions.parents = true
		case arg == "--link":
			spec.copyOptions.link = true
		case strings.HasPrefix(arg, "--keep-git-dir="):
			value, err := strconv.ParseBool(b.expand(strings.TrimPrefix(arg, "--keep-git-dir=")))
			if err != nil {
				return transferSpec{}, fmt.Errorf("parse --keep-git-dir: %w", err)
			}
			spec.copyOptions.keepGitDir = value
		case strings.HasPrefix(arg, "--checksum="):
			checksum, err := parseChecksumSpec(b.expand(strings.TrimPrefix(arg, "--checksum=")))
			if err != nil {
				return transferSpec{}, err
			}
			spec.copyOptions.checksum = checksum
		case strings.HasPrefix(arg, "--unpack="):
			value, err := strconv.ParseBool(b.expand(strings.TrimPrefix(arg, "--unpack=")))
			if err != nil {
				return transferSpec{}, fmt.Errorf("parse --unpack: %w", err)
			}
			spec.copyOptions.unpack = &value
		case strings.HasPrefix(arg, "--"):
			return transferSpec{}, fmt.Errorf("unsupported flag %q", arg)
		default:
			filtered = append(filtered, arg)
		}
	}
	if len(excludePatterns) > 0 {
		matcher, err := patternmatcher.New(excludePatterns)
		if err != nil {
			return transferSpec{}, fmt.Errorf("parse --exclude: %w", err)
		}
		spec.copyOptions.excludeMatcher = matcher
	}

	if len(filtered) < 2 {
		return transferSpec{}, fmt.Errorf("COPY requires src and dest")
	}
	spec.srcs = filtered[:len(filtered)-1]
	spec.dst = b.expand(filtered[len(filtered)-1])
	return spec, nil
}

func (b *builder) resolveRemoteTransferRoot(ref string) (string, error) {
	if root, ok := b.remoteRootfs[ref]; ok {
		return root, nil
	}
	pulled, err := b.pullImage(ref)
	if err != nil {
		return "", err
	}
	root := filepath.Join(b.stagesRoot, fmt.Sprintf("remote-%d", len(b.remoteRootfs)))
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", err
	}
	if err := pulled.ExtractToDir(root); err != nil {
		return "", err
	}
	b.remoteRootfs[ref] = root
	return root, nil
}

func (b *builder) collectTransferSources(spec transferSpec, options transferOptions) ([]transferSource, error) {
	var out []transferSource
	// BuildKit semantics: when COPY receives multiple src patterns and at
	// least one of them uses wildcards, a pattern that matches nothing is
	// tolerated as long as SOME src still contributes files. A non-wildcard
	// src that is missing is still an error.
	for _, raw := range spec.srcs {
		expandedRaw := b.expand(raw)
		if options.allowRemote {
			if parsedURL, ok := parseRemoteURL(expandedRaw); ok {
				source := transferSource{raw: raw, remoteURL: parsedURL}
				include, err := b.includeTransferSource(spec, source)
				if err != nil {
					return nil, err
				}
				if include {
					out = append(out, source)
				}
				continue
			}
		}

		sources, err := expandLocalTransferSources(spec.srcRoot, expandedRaw, raw)
		if err != nil {
			// Wildcard patterns: missing matches are non-fatal in
			// multi-src COPY. Only fail if every glob ends up empty.
			if hasWildcards(expandedRaw) && len(spec.srcs) > 1 {
				continue
			}
			return nil, err
		}
		kept := 0
		for _, src := range sources {
			include, err := b.includeTransferSource(spec, src)
			if err != nil {
				return nil, err
			}
			if include {
				out = append(out, src)
				kept++
			}
		}
		if kept == 0 && !hasWildcards(expandedRaw) {
			return nil, fmt.Errorf("source %q not found in build context or excluded by .dockerignore", raw)
		}
	}
	return out, nil
}

func (b *builder) includeTransferSource(spec transferSpec, src transferSource) (bool, error) {
	if spec.fromContext && src.rel != "" {
		ignored, err := b.contextFilter.ignored(src.rel)
		if err != nil {
			return false, err
		}
		if ignored {
			return false, nil
		}
	}
	excluded, err := spec.copyOptions.excluded(src.rootExcludePath())
	if err != nil {
		return false, err
	}
	if excluded {
		return false, nil
	}
	return true, nil
}

func expandLocalTransferSources(root, value, raw string) ([]transferSource, error) {
	var (
		err   error
		paths []string
	)
	if hasWildcards(value) {
		paths, err = globTransferPattern(root, value)
		if err != nil {
			return nil, err
		}
	} else {
		patternPath, err := resolveCopySource(root, value)
		if err != nil {
			return nil, err
		}
		paths = []string{patternPath}
	}

	sources := make([]transferSource, 0, len(paths))
	for _, absPath := range paths {
		info, err := os.Lstat(absPath)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, absPath)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("source %q escapes build root", raw)
		}
		sources = append(sources, transferSource{
			raw:  raw,
			abs:  absPath,
			rel:  normalizeMatcherPath(rel),
			info: info,
		})
	}
	return sources, nil
}

func globTransferPattern(root, pattern string) ([]string, error) {
	normalizedPattern := normalizeMatcherPath(strings.TrimPrefix(pattern, "/"))
	if normalizedPattern == "." {
		return []string{root}, nil
	}
	matches := make([]string, 0)
	if err := filepath.WalkDir(root, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, absPath)
		if err != nil {
			return err
		}
		normalizedRel := normalizeMatcherPath(rel)
		ok, err := matchTransferPattern(normalizedPattern, normalizedRel)
		if err != nil {
			return err
		}
		if ok {
			matches = append(matches, absPath)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("source %q not found", pattern)
	}
	return matches, nil
}

func matchTransferPattern(pattern, rel string) (bool, error) {
	patternSegs := splitTransferPattern(pattern)
	relSegs := splitTransferPattern(rel)
	return matchTransferSegments(patternSegs, relSegs)
}

func splitTransferPattern(value string) []string {
	normalized := normalizeMatcherPath(value)
	if normalized == "." || normalized == "" {
		return nil
	}
	return strings.Split(normalized, "/")
}

func matchTransferSegments(patternSegs, relSegs []string) (bool, error) {
	if len(patternSegs) == 0 {
		return len(relSegs) == 0, nil
	}
	if patternSegs[0] == "**" {
		if len(patternSegs) == 1 {
			return true, nil
		}
		for idx := 0; idx <= len(relSegs); idx++ {
			matched, err := matchTransferSegments(patternSegs[1:], relSegs[idx:])
			if err != nil {
				return false, err
			}
			if matched {
				return true, nil
			}
		}
		return false, nil
	}
	if len(relSegs) == 0 {
		return false, nil
	}
	matched, err := path.Match(patternSegs[0], relSegs[0])
	if err != nil {
		return false, err
	}
	if !matched {
		return false, nil
	}
	return matchTransferSegments(patternSegs[1:], relSegs[1:])
}

func parseRemoteURL(value string) (*url.URL, bool) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, false
	}
	if parsed.Host == "" {
		return nil, false
	}
	return parsed, true
}

func parseChecksumSpec(value string) (*checksumSpec, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("parse --checksum %q: expected algorithm:digest", value)
	}
	algorithm := strings.ToLower(strings.TrimSpace(parts[0]))
	digest := strings.ToLower(strings.TrimSpace(parts[1]))
	if algorithm != "sha256" {
		return nil, fmt.Errorf("parse --checksum %q: only sha256 is supported", value)
	}
	if len(digest) != sha256.Size*2 {
		return nil, fmt.Errorf("parse --checksum %q: invalid sha256 length", value)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, fmt.Errorf("parse --checksum %q: %w", value, err)
	}
	return &checksumSpec{algorithm: algorithm, expected: digest}, nil
}

func hasWildcards(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func normalizeMatcherPath(value string) string {
	cleaned := filepath.Clean(value)
	cleaned = strings.TrimPrefix(cleaned, "."+string(filepath.Separator))
	cleaned = strings.TrimPrefix(cleaned, string(filepath.Separator))
	cleaned = filepath.ToSlash(cleaned)
	if cleaned == "" {
		return "."
	}
	return cleaned
}

func resolveCopyOwnership(rootfs, value string) (*ownershipSpec, error) {
	resolved, err := usercfg.Resolve(rootfs, value)
	if err != nil {
		return nil, fmt.Errorf("resolve --chown %q: %w", value, err)
	}
	return &ownershipSpec{UID: int(resolved.UID), GID: int(resolved.GID)}, nil
}

func parseCopyMode(value string) (*os.FileMode, error) {
	parsed, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return nil, fmt.Errorf("parse --chmod %q: %w", value, err)
	}
	mode := os.FileMode(parsed)
	return &mode, nil
}

func ensureDestinationDir(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("destination %s is not a directory", path)
		}
		return nil
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	return nil
}

func resolveFileTarget(dstAbs string, dirHint bool, baseName string) (string, error) {
	if dirHint {
		if err := os.MkdirAll(dstAbs, 0755); err != nil {
			return "", err
		}
		return filepath.Join(dstAbs, baseName), nil
	}
	if info, err := os.Stat(dstAbs); err == nil && info.IsDir() {
		return filepath.Join(dstAbs, baseName), nil
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0755); err != nil {
		return "", err
	}
	return dstAbs, nil
}

func resolveTransferTarget(dstAbs string, dirHint bool, src transferSource, opts copyOptions) (string, error) {
	if !opts.parents {
		return resolveFileTarget(dstAbs, dirHint, src.baseName())
	}
	rel := src.parentRelativePath()
	if rel == "." || rel == "" {
		return resolveFileTarget(dstAbs, true, src.baseName())
	}
	targetPath := filepath.Join(dstAbs, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return "", err
	}
	return targetPath, nil
}

func downloadToPath(remoteURL *url.URL, targetPath string, opts copyOptions) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(remoteURL.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	writer := io.Writer(file)
	hasher := sha256.New()
	if opts.checksum != nil {
		writer = io.MultiWriter(file, hasher)
	}
	if _, err := io.Copy(writer, resp.Body); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if opts.checksum != nil {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != opts.checksum.expected {
			_ = os.Remove(targetPath)
			return fmt.Errorf("checksum mismatch: got sha256:%s want sha256:%s", got, opts.checksum.expected)
		}
	}
	return applyCopyAdjustments(targetPath, opts)
}

func extractArchiveToDir(archivePath, dstDir string, opts copyOptions) error {
	if err := ensureDestinationDir(dstDir); err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "gocracker-add-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := untarArchivePath(archivePath, tmpDir); err != nil {
		return err
	}
	archiveOpts := opts
	archiveOpts.preserveOwnership = true
	_, err = copyDirContentsWithOptions(tmpDir, dstDir, archiveOpts)
	return err
}

func (b *builder) copyContextDirContents(src transferSource, dstDir string, opts copyOptions) (int, error) {
	return b.copyDirectorySource(src, dstDir, opts, true)
}

func (b *builder) copyDirectorySource(src transferSource, dstDir string, opts copyOptions, fromContext bool) (int, error) {
	rootTarget := dstDir
	if opts.parents {
		rel := src.parentRelativePath()
		if rel != "." && rel != "" {
			rootTarget = filepath.Join(dstDir, filepath.FromSlash(rel))
			if err := mkdirFromSource(src.abs, rootTarget, opts); err != nil {
				return 0, err
			}
		} else if err := ensureDestinationDir(dstDir); err != nil {
			return 0, err
		}
	} else {
		if info, err := os.Lstat(dstDir); err == nil {
			if !info.IsDir() {
				return 0, fmt.Errorf("destination %s is not a directory", dstDir)
			}
		} else if os.IsNotExist(err) {
			if err := mkdirFromSource(src.abs, dstDir, opts); err != nil {
				return 0, err
			}
		} else {
			return 0, err
		}
	}

	copied := 0
	err := filepath.Walk(src.abs, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relToSrc, err := filepath.Rel(src.abs, path)
		if err != nil {
			return err
		}
		if relToSrc == "." {
			return nil
		}
		if fromContext {
			contextRel := normalizeMatcherPath(filepath.Join(src.rel, relToSrc))
			ignored, err := b.contextFilter.ignored(contextRel)
			if err != nil {
				return err
			}
			if ignored {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		excluded, err := opts.excluded(relToSrc)
		if err != nil {
			return err
		}
		if excluded {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		targetPath := filepath.Join(rootTarget, relToSrc)
		if info.IsDir() {
			if err := mkdirFromSource(path, targetPath, opts); err != nil {
				return err
			}
		} else {
			if err := copyPathWithOptions(path, targetPath, opts); err != nil {
				return err
			}
		}
		copied++
		return nil
	})
	return copied, err
}

func copyDirContentsWithOptions(srcDir, dstDir string, opts copyOptions) (int, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return 0, err
	}
	copied := 0
	for _, entry := range entries {
		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())
		if err := copyPathWithOptions(srcPath, dstPath, opts); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, nil
}

func mkdirFromSource(srcPath, dstPath string, opts copyOptions) error {
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if err := os.MkdirAll(dstPath, mode); err != nil {
		return err
	}
	if opts.preserveOwnership {
		if err := applyOwnership(dstPath, info, true); err != nil {
			return err
		}
	}
	return applyPathAdjustments(dstPath, info, opts)
}

func copyPathWithOptions(src, dst string, opts copyOptions) error {
	if opts.preserveOwnership {
		if err := copyPathPreserveMetadata(src, dst); err != nil {
			return err
		}
	} else {
		if err := copyPath(src, dst, false); err != nil {
			return err
		}
	}
	return applyCopyAdjustments(dst, opts)
}

func applyCopyAdjustments(path string, opts copyOptions) error {
	if opts.chown == nil && opts.chmod == nil {
		return nil
	}
	return filepath.Walk(path, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return applyPathAdjustments(current, info, opts)
	})
}

func applyPathAdjustments(path string, info os.FileInfo, opts copyOptions) error {
	if opts.chown != nil {
		if err := os.Lchown(path, opts.chown.UID, opts.chown.GID); err != nil && !os.IsPermission(err) {
			return err
		}
	}
	if opts.chmod != nil && info.Mode()&os.ModeSymlink == 0 {
		if err := os.Chmod(path, *opts.chmod); err != nil {
			return err
		}
	}
	return nil
}

func isLocalArchivePath(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	reader, err := decompressArchiveStream(file)
	if err != nil {
		return false
	}
	defer reader.Close()

	tr := tar.NewReader(reader)
	_, err = tr.Next()
	return err == nil
}

func untarArchivePath(src, dst string) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()

	reader, err := decompressArchiveStream(file)
	if err != nil {
		return err
	}
	defer reader.Close()

	return untar(reader, dst)
}

func decompressArchiveStream(r io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(r)
	header, err := buffered.Peek(6)
	if err != nil && err != io.EOF {
		return nil, err
	}
	switch {
	case len(header) >= 2 && bytes.Equal(header[:2], []byte{0x1f, 0x8b}):
		return gzip.NewReader(buffered)
	case len(header) >= 3 && bytes.Equal(header[:3], []byte("BZh")):
		return io.NopCloser(bzip2.NewReader(buffered)), nil
	case len(header) >= 6 && bytes.Equal(header[:6], []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}):
		reader, err := xz.NewReader(buffered)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(reader), nil
	default:
		return io.NopCloser(buffered), nil
	}
}

func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel := filepath.Clean(hdr.Name)
		if rel == "." {
			continue
		}
		if strings.HasPrefix(rel, "..") {
			return fmt.Errorf("archive path escapes destination: %s", hdr.Name)
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0111); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0200)
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, tr); err != nil {
				file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.RemoveAll(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			_ = os.RemoveAll(target)
			if err := os.Link(filepath.Join(dst, filepath.Clean(hdr.Linkname)), target); err != nil {
				return err
			}
		default:
			continue
		}

		if err := applyTarHeaderOwnership(target, hdr); err != nil {
			return err
		}
	}
}

func applyTarHeaderOwnership(path string, hdr *tar.Header) error {
	if hdr.Uid == 0 && hdr.Gid == 0 {
		return nil
	}
	if err := os.Lchown(path, hdr.Uid, hdr.Gid); err != nil && !os.IsPermission(err) && err != syscall.EPERM {
		return err
	}
	return nil
}
