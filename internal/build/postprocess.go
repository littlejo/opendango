package build

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// made is what a stage made, for the post-processors (OUTPUT --format=
// manifest, checksum, compress, and plugin://): the template or the
// plugin's artifact, and the files (XVA exports, the plugin's files).
type made struct {
	ID          string
	BuilderID   string
	BuilderType string
	Files       []string
}

// FindPostProcessor looks for the plugin of a post-processor type (vagrant,
// docker-import) like FindPlugin, and checks that it has it.
func FindPostProcessor(ctx context.Context, source string) (*packer.Plugin, string, error) {
	plugins := packer.Discover(packer.SearchDirs(), filepath.SplitList(os.Getenv("PATH")))
	pl, name, err := packer.Find(plugins, source)
	if err != nil {
		return nil, "", err
	}
	d, err := pl.Describe(ctx)
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains(d.PostProcessors, name) {
		return nil, "", fmt.Errorf("plugin: %s has no post-processor %q (post-processors: %s)", pl.Path, name, strings.Join(d.PostProcessors, ", "))
	}
	return pl, name, nil
}

// postProcess runs the post-processors of a stage, in the order of the
// Dangofile, on what it made.
func postProcess(ctx context.Context, p *plan.Plan, stage *plan.Stage, m made, e *executor, opts Options) error {
	for _, out := range stage.Outputs {
		if !out.PostProcess() {
			continue
		}
		var err error
		path := out.Path
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(opts.ContextDir, path)
		}
		switch out.Kind {
		case instructions.OutputManifest:
			e.say("Adding the build to the manifest %s", out.Path)
			err = opts.manifests.add(path, manifestBuild(stage, m, opts))
		case instructions.OutputChecksum:
			e.say("Writing the %s checksums to %s", out.Type, out.Path)
			err = writeChecksums(path, out.Type, stage, m)
		case instructions.OutputArchive:
			e.say("Compressing the files to %s", out.Path)
			err = writeArchive(path, stage, m)
		case instructions.OutputPlugin:
			err = pluginPostProcess(ctx, stage, out, m, e, opts)
		}
		if err != nil {
			return &plan.Error{File: p.File, Line: out.Line(), Err: fmt.Errorf("OUTPUT: %w", err)}
		}
	}
	return nil
}

// noFiles is the error of a post-processor that needs files when the
// artifact has none (a template, an AMI).
func noFiles(stage *plan.Stage, m made) error {
	return fmt.Errorf("the artifact of stage %s has no file (%s): export one first (such as OUTPUT --format=xva)", stage.Name(), m.ID)
}

// writeChecksums writes "<hex>  <name>" for each file of the artifact, the
// format of sha256sum (sha256sum -c checks it); names are relative to the
// checksum file when they are under its directory.
func writeChecksums(path, typ string, stage *plan.Stage, m made) error {
	if len(m.Files) == 0 {
		return noFiles(stage, m)
	}
	var b strings.Builder
	for _, f := range m.Files {
		h := newHash(typ)
		r, err := os.Open(f)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, r)
		r.Close()
		if err != nil {
			return err
		}
		name := f
		if rel, err := filepath.Rel(filepath.Dir(path), f); err == nil && !strings.HasPrefix(rel, "..") {
			name = rel
		}
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h.Sum(nil)), filepath.ToSlash(name))
	}
	return writeFile(path, []byte(b.String()))
}

func newHash(typ string) hash.Hash {
	switch typ {
	case "md5":
		return md5.New()
	case "sha1":
		return sha1.New()
	case "sha224":
		return sha256.New224()
	case "sha384":
		return sha512.New384()
	case "sha512":
		return sha512.New()
	default:
		return sha256.New()
	}
}

// writeArchive puts the files of the artifact at the root of an archive,
// whose format the extension gives (.tar, .tar.gz or .tgz, .zip). A
// directory keeps its tree.
func writeArchive(path string, stage *plan.Stage, m made) error {
	if len(m.Files) == 0 {
		return noFiles(stage, m)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".opendango-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if strings.HasSuffix(path, ".zip") {
		err = writeZip(tmp, m.Files)
	} else {
		var w io.Writer = tmp
		var gz *gzip.Writer
		if strings.HasSuffix(path, ".gz") || strings.HasSuffix(path, ".tgz") {
			gz = gzip.NewWriter(tmp)
			w = gz
		}
		err = writeTar(w, m.Files)
		if gz != nil {
			if cerr := gz.Close(); err == nil {
				err = cerr
			}
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// archiveWalk calls fn for every file and directory of the sources, with
// its name in the archive: the source's base name, then its tree.
func archiveWalk(files []string, fn func(name, p string, info os.FileInfo) error) error {
	for _, f := range files {
		base := filepath.Dir(f)
		err := filepath.Walk(f, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			return fn(filepath.ToSlash(rel), p, info)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func writeTar(w io.Writer, files []string) error {
	tw := tar.NewWriter(w)
	err := archiveWalk(files, func(name, p string, info os.FileInfo) error {
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = name
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(tw, p)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func writeZip(w io.Writer, files []string) error {
	zw := zip.NewWriter(w)
	err := archiveWalk(files, func(name, p string, info os.FileInfo) error {
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name, h.Method = name, zip.Deflate
		if info.IsDir() {
			h.Name += "/"
		}
		fw, err := zw.CreateHeader(h)
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		return copyFile(fw, p)
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

func copyFile(w io.Writer, p string) error {
	r, err := os.Open(p)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}

// writeFile writes data to path through a temporary file renamed on
// success, creating the directory.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".opendango-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// pluginPostProcess runs a plugin's post-processor (OUTPUT plugin://...) on
// what the stage made.
func pluginPostProcess(ctx context.Context, stage *plan.Stage, out *instructions.Output, m made, e *executor, opts Options) error {
	cfg, err := packer.ParseConfig(out.Source, out.Settings)
	if err != nil {
		return err
	}
	e.say("Looking for the post-processor %s", cfg.Component)
	pl, name, err := opts.FindPostProcessor(ctx, cfg.Component)
	if err != nil {
		return err
	}
	core := map[string]any{
		"packer_build_name":          stage.Name(),
		"packer_builder_type":        m.BuilderType,
		"packer_core_version":        "opendango",
		"packer_debug":               false,
		"packer_force":               false,
		"packer_on_error":            "cleanup",
		"packer_user_variables":      map[string]string{},
		"packer_sensitive_variables": cfg.Secrets,
	}
	ui := &packer.Ui{Print: func(line string) { e.say("%s", line) }, Secrets: cfg.Secrets}
	files, err := packer.PostProcess(ctx, pl, name, opts.ContextDir, ui, packer.Input{ID: m.ID, BuilderID: m.BuilderID, Files: m.Files}, cfg.Settings, core)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		e.say("Post-processor %s made %s", cfg.Component, strings.Join(files, ", "))
	}
	return nil
}

// manifests writes the manifest files of a build: a stage adds its build
// to the file, like Packer's manifest post-processor (same JSON format, so
// that its readers work), keeping the builds of the previous runs.
type manifests struct {
	mu      sync.Mutex
	runUUID string
}

type manifestFile struct {
	Builds      []manifestEntry `json:"builds"`
	LastRunUUID string          `json:"last_run_uuid"`
}

type manifestEntry struct {
	Name          string          `json:"name"`
	BuilderType   string          `json:"builder_type"`
	BuildTime     int64           `json:"build_time"`
	Files         []manifestFiles `json:"files"`
	ArtifactID    string          `json:"artifact_id"`
	PackerRunUUID string          `json:"packer_run_uuid"`
	CustomData    map[string]any  `json:"custom_data"`
}

type manifestFiles struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func newManifests() *manifests {
	b := make([]byte, 16)
	rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return &manifests{runUUID: h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]}
}

func manifestBuild(stage *plan.Stage, m made, opts Options) manifestEntry {
	entry := manifestEntry{Name: stage.Name(), BuilderType: m.BuilderType, BuildTime: time.Now().Unix(), ArtifactID: m.ID, Files: []manifestFiles{}}
	for _, f := range m.Files {
		name := f
		if rel, err := filepath.Rel(opts.ContextDir, f); err == nil && !strings.HasPrefix(rel, "..") {
			name = rel
		}
		var size int64
		if info, err := os.Stat(f); err == nil {
			size = info.Size()
		}
		entry.Files = append(entry.Files, manifestFiles{Name: filepath.ToSlash(name), Size: size})
	}
	return entry
}

func (ms *manifests) add(path string, entry manifestEntry) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	var mf manifestFile
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &mf); err != nil {
			return fmt.Errorf("%s is not a manifest: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entry.PackerRunUUID = ms.runUUID
	mf.Builds = append(mf.Builds, entry)
	mf.LastRunUUID = ms.runUUID
	data, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}
