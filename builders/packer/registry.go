package packer

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// Registry finds and installs plugins: the official HashiCorp plugins from
// releases.hashicorp.com (they are not published on GitHub), the others
// from the GitHub releases of their repository.
type Registry struct {
	HashiCorpAPI string
	GitHubAPI    string
	// Token is sent to GitHubAPI only (not to the downloads): it raises the
	// rate limits of the API.
	Token    string
	Client   *http.Client
	OS, Arch string
	// Dir is where plugins are installed, in the layout of `packer plugins
	// install`.
	Dir string
}

// NewRegistry returns the registry of the public services, installing in
// Packer's plugin directory, with GITHUB_TOKEN when it is set.
func NewRegistry() (*Registry, error) {
	dir := PluginDir()
	if dir == "" {
		return nil, errors.New("packer: no plugin directory: set PACKER_CONFIG_DIR or HOME")
	}
	return &Registry{
		HashiCorpAPI: "https://api.releases.hashicorp.com",
		GitHubAPI:    "https://api.github.com",
		Token:        os.Getenv("GITHUB_TOKEN"),
		Client:       http.DefaultClient,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Dir:          dir,
	}, nil
}

// Source is the address of a plugin, github.com/<owner>/<name>, for the
// repository <owner>/packer-plugin-<name>, like in Packer.
type Source struct{ Owner, Name string }

func (s Source) String() string { return "github.com/" + s.Owner + "/" + s.Name }

func (s Source) repo() string { return s.Owner + "/packer-plugin-" + s.Name }

var (
	pluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	ownerName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	versionRe  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*(-[0-9A-Za-z.-]+)?$`)
)

// ParseRef splits a plugin reference: a name (qemu) or a source
// (github.com/vatesfr/xenserver), with an optional @version (a leading v is
// dropped). The source is nil for a bare name, which Resolve looks up.
func ParseRef(ref string) (name string, src *Source, version string, err error) {
	ref, version, hasVersion := strings.Cut(ref, "@")
	version = strings.TrimPrefix(version, "v")
	if hasVersion && !versionRe.MatchString(version) {
		return "", nil, "", fmt.Errorf("invalid plugin version %q", version)
	}
	if !strings.Contains(ref, "/") {
		if !pluginName.MatchString(ref) {
			return "", nil, "", fmt.Errorf("invalid plugin name %q: lower-case letters, digits and -", ref)
		}
		return ref, nil, version, nil
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 3 || parts[0] != "github.com" || !ownerName.MatchString(parts[1]) || !pluginName.MatchString(parts[2]) {
		return "", nil, "", fmt.Errorf("invalid plugin source %q: expected github.com/<owner>/<name>", ref)
	}
	return parts[2], &Source{Owner: parts[1], Name: parts[2]}, version, nil
}

// Resolve returns the source of a plugin name: the official HashiCorp
// plugin of that name, or else the only GitHub repository named
// packer-plugin-<name> that is not a fork.
func (r *Registry) Resolve(ctx context.Context, name string) (Source, error) {
	official, err := r.official(ctx)
	if err != nil {
		return Source{}, err
	}
	if slices.Contains(official, name) {
		return Source{Owner: "hashicorp", Name: name}, nil
	}
	repos, err := r.searchGitHub(ctx, "packer-plugin-"+name)
	if err != nil {
		return Source{}, err
	}
	var found []Source
	for _, repo := range repos {
		if src, ok := repo.source(); ok && src.Name == name && !repo.Fork {
			found = append(found, src)
		}
	}
	switch len(found) {
	case 0:
		return Source{}, fmt.Errorf("no plugin named %q: look for one with `opendango plugin search`", name)
	case 1:
		return found[0], nil
	}
	var names []string
	for _, s := range found {
		names = append(names, s.String())
	}
	return Source{}, fmt.Errorf("several plugins are named %q, give the source: %s", name, strings.Join(names, ", "))
}

// Install downloads a release of the plugin (the latest when version is
// empty) for r.OS and r.Arch, checks it against the release's SHA256SUMS
// and describe, and installs it in r.Dir with its _SHA256SUM file. An
// installed binary is kept as is: installed is false then.
func (r *Registry) Install(ctx context.Context, src Source, version string) (pl *Plugin, installed bool, err error) {
	rel, err := r.release(ctx, src, version)
	if err != nil {
		return nil, false, err
	}
	dir := filepath.Join(r.Dir, "github.com", src.Owner, src.Name)
	bin := fmt.Sprintf("packer-plugin-%s_v%s_x%s.0_%s_%s", src.Name, rel.version, apiMajor, r.OS, r.Arch)
	if r.OS == "windows" {
		bin += ".exe"
	}
	dst := filepath.Join(dir, bin)
	if _, err := os.Stat(dst); err == nil {
		return &Plugin{Name: src.Name, Version: rel.version, Path: dst}, false, nil
	}

	sums, err := r.get(ctx, rel.sumsURL, false)
	if err != nil {
		return nil, false, err
	}
	want, err := checksumOf(sums, rel.zipName)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", rel.sumsURL, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	archive, err := os.CreateTemp(dir, ".download-*.zip")
	if err != nil {
		return nil, false, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err := r.download(ctx, rel.zipURL, archive, want); err != nil {
		return nil, false, err
	}
	sum, err := extract(archive, src.Name, rel.version, r.OS, r.Arch, dst)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", rel.zipURL, err)
	}
	pl = &Plugin{Name: src.Name, Version: rel.version, Path: dst}
	err = os.WriteFile(dst+"_SHA256SUM", []byte(sum), 0o644)
	if err == nil {
		_, err = pl.Describe(ctx)
	}
	if err != nil {
		os.Remove(dst)
		os.Remove(dst + "_SHA256SUM")
		return nil, false, err
	}
	return pl, true, nil
}

// Remove deletes the installed versions of a plugin (all of them when
// version is empty) from r.Dir, with their _SHA256SUM files. A nil source
// matches every owner, which must then be unique.
func (r *Registry) Remove(name string, src *Source, version string) ([]string, error) {
	owner := "*"
	if src != nil {
		owner = src.Owner
	}
	v := "*"
	if version != "" {
		v = version
	}
	matches, err := filepath.Glob(filepath.Join(r.Dir, "github.com", owner, name, "packer-plugin-"+name+"_v"+v+"_x*"))
	if err != nil {
		return nil, err
	}
	var bins []string
	owners := map[string]bool{}
	for _, m := range matches {
		if pl, ok := parseName(m); ok && pl.Name == name && (version == "" || pl.Version == version) {
			bins = append(bins, m)
			owners[filepath.Base(filepath.Dir(filepath.Dir(m)))] = true
		}
	}
	if len(bins) == 0 {
		return nil, fmt.Errorf("plugin %s is not installed in %s", strings.TrimSuffix(name+"@"+version, "@"), r.Dir)
	}
	if len(owners) > 1 {
		var sources []string
		for o := range owners {
			sources = append(sources, Source{Owner: o, Name: name}.String())
		}
		sort.Strings(sources)
		return nil, fmt.Errorf("several plugins are named %q, give the source: %s", name, strings.Join(sources, ", "))
	}
	for _, b := range bins {
		if err := os.Remove(b); err != nil {
			return nil, err
		}
		os.Remove(b + "_SHA256SUM")
		// Empty directories only: the errors are expected otherwise.
		os.Remove(filepath.Dir(b))
		os.Remove(filepath.Dir(filepath.Dir(b)))
	}
	return bins, nil
}

// SearchResult is a plugin found by Search.
type SearchResult struct {
	Source      Source
	Official    bool
	Fork        bool
	Description string
	Stars       int
}

// Search lists the official plugins whose name contains term, then the
// GitHub repositories named packer-plugin-<name> whose name contains it,
// the most starred first: archived repositories are left out, forks are
// kept when they have a star (some maintained plugins are forks, most forks
// are personal copies). When GitHub fails, the official results come
// with the error.
func (r *Registry) Search(ctx context.Context, term string) ([]SearchResult, error) {
	term = strings.ToLower(term)
	official, err := r.official(ctx)
	if err != nil {
		return nil, err
	}
	var results []SearchResult
	seen := map[Source]bool{}
	for _, name := range official {
		if strings.Contains(name, term) {
			src := Source{Owner: "hashicorp", Name: name}
			results = append(results, SearchResult{Source: src, Official: true})
			seen[src] = true
		}
	}
	repos, err := r.searchGitHub(ctx, "packer-plugin "+term+" fork:true")
	if err != nil {
		return results, err
	}
	var community []SearchResult
	for _, repo := range repos {
		src, ok := repo.source()
		if !ok || repo.Archived || (repo.Fork && repo.Stars == 0) || !strings.Contains(src.Name, term) {
			continue
		}
		if seen[src] {
			for i := range results {
				if results[i].Source == src {
					results[i].Description, results[i].Stars = repo.Description, repo.Stars
				}
			}
			continue
		}
		seen[src] = true
		community = append(community, SearchResult{Source: src, Fork: repo.Fork, Description: repo.Description, Stars: repo.Stars})
	}
	sort.SliceStable(community, func(i, j int) bool { return community[i].Stars > community[j].Stars })
	return append(results, community...), nil
}

// official returns the names of the official HashiCorp plugins.
func (r *Registry) official(ctx context.Context) ([]string, error) {
	var products []string
	if err := r.getJSON(ctx, r.HashiCorpAPI+"/v1/products", false, &products); err != nil {
		return nil, err
	}
	var names []string
	for _, p := range products {
		if name, ok := strings.CutPrefix(p, "packer-plugin-"); ok {
			names = append(names, name)
		}
	}
	return names, nil
}

type gitHubRepo struct {
	Name        string                 `json:"name"`
	Owner       struct{ Login string } `json:"owner"`
	Description string                 `json:"description"`
	Stars       int                    `json:"stargazers_count"`
	Fork        bool                   `json:"fork"`
	Archived    bool                   `json:"archived"`
}

// source returns the plugin source of a packer-plugin-<name> repository.
func (g gitHubRepo) source() (Source, bool) {
	name, ok := strings.CutPrefix(strings.ToLower(g.Name), "packer-plugin-")
	if !ok || !pluginName.MatchString(name) || !ownerName.MatchString(g.Owner.Login) {
		return Source{}, false
	}
	return Source{Owner: g.Owner.Login, Name: name}, true
}

// searchGitHub searches the repository names.
func (r *Registry) searchGitHub(ctx context.Context, terms string) ([]gitHubRepo, error) {
	var res struct {
		Items []gitHubRepo `json:"items"`
	}
	u := r.GitHubAPI + "/search/repositories?per_page=100&q=" + url.QueryEscape(terms+" in:name")
	if err := r.getJSON(ctx, u, true, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// release is the archive of a plugin version for one platform.
type release struct {
	version, zipName, zipURL, sumsURL string
}

func (r *Registry) release(ctx context.Context, src Source, version string) (*release, error) {
	if src.Owner == "hashicorp" {
		official, err := r.official(ctx)
		if err != nil {
			return nil, err
		}
		if slices.Contains(official, src.Name) {
			return r.hashiCorpRelease(ctx, src, version)
		}
	}
	return r.gitHubRelease(ctx, src, version)
}

func (r *Registry) hashiCorpRelease(ctx context.Context, src Source, version string) (*release, error) {
	which := "latest"
	if version != "" {
		which = version
	}
	var res struct {
		Version string `json:"version"`
		Builds  []struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
			URL  string `json:"url"`
		} `json:"builds"`
		Shasums string `json:"url_shasums"`
	}
	u := r.HashiCorpAPI + "/v1/releases/packer-plugin-" + src.Name + "/" + url.PathEscape(which)
	if err := r.getJSON(ctx, u, false, &res); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("%s has no version %s", src, version)
		}
		return nil, err
	}
	if !versionRe.MatchString(res.Version) {
		return nil, fmt.Errorf("%s: invalid version %q", u, res.Version)
	}
	for _, b := range res.Builds {
		if b.OS == r.OS && b.Arch == r.Arch {
			return &release{version: res.Version, zipName: path.Base(b.URL), zipURL: b.URL, sumsURL: res.Shasums}, nil
		}
	}
	return nil, fmt.Errorf("%s %s has no build for %s/%s", src, res.Version, r.OS, r.Arch)
}

func (r *Registry) gitHubRelease(ctx context.Context, src Source, version string) (*release, error) {
	u := r.GitHubAPI + "/repos/" + src.repo() + "/releases/latest"
	if version != "" {
		u = r.GitHubAPI + "/repos/" + src.repo() + "/releases/tags/v" + url.PathEscape(version)
	}
	var res struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := r.getJSON(ctx, u, true, &res); err != nil {
		if errors.Is(err, errNotFound) {
			if version != "" {
				return nil, fmt.Errorf("%s has no release v%s (or no repository %s)", src, version, src.repo())
			}
			return nil, fmt.Errorf("%s has no release (or no repository %s)", src, src.repo())
		}
		return nil, err
	}
	v := strings.TrimPrefix(res.Tag, "v")
	if !versionRe.MatchString(v) {
		return nil, fmt.Errorf("%s: invalid release tag %q", src, res.Tag)
	}
	prefix := "packer-plugin-" + src.Name + "_v" + v + "_"
	zipRe := regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + `x` + apiMajor + `\.[0-9]+_` + regexp.QuoteMeta(r.OS+"_"+r.Arch) + `\.zip$`)
	rel := &release{version: v}
	for _, a := range res.Assets {
		switch {
		case zipRe.MatchString(a.Name):
			rel.zipName, rel.zipURL = a.Name, a.URL
		case a.Name == prefix+"SHA256SUMS":
			rel.sumsURL = a.URL
		}
	}
	if rel.zipURL == "" {
		return nil, fmt.Errorf("%s %s has no build for %s/%s (plugin API x%s)", src, v, r.OS, r.Arch, apiMajor)
	}
	if rel.sumsURL == "" {
		return nil, fmt.Errorf("%s %s has no %sSHA256SUMS file", src, v, prefix)
	}
	return rel, nil
}

// checksumOf finds the sha256 of name in a SHA256SUMS file.
func checksumOf(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != 2*sha256.Size {
				return "", fmt.Errorf("invalid sha256 for %s", name)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no sha256 for %s", name)
}

// download writes the body of u to f and checks its sha256.
func (r *Registry) download(ctx context.Context, u string, f *os.File, want string) error {
	resp, err := r.do(ctx, u, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return fmt.Errorf("downloading %s: %w", u, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: sha256 %s, want %s (SHA256SUMS)", u, got, want)
	}
	return nil
}

// extract writes the plugin binary of the archive to dst (mode 0755) and
// returns its sha256. The archive must hold exactly one binary named like
// an installed plugin of that name, version and platform.
func extract(archive *os.File, name, version, goos, goarch, dst string) (string, error) {
	info, err := archive.Stat()
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(archive, info.Size())
	if err != nil {
		return "", err
	}
	binRe := regexp.MustCompile("^" + regexp.QuoteMeta("packer-plugin-"+name+"_v"+version+"_") + `x` + apiMajor + `\.[0-9]+_` + regexp.QuoteMeta(goos+"_"+goarch) + `(\.exe)?$`)
	var bin *zip.File
	for _, f := range zr.File {
		if !f.Mode().IsRegular() || !binRe.MatchString(path.Base(f.Name)) {
			continue
		}
		if bin != nil {
			return "", errors.New("several plugin binaries in the archive")
		}
		bin = f
	}
	if bin == nil {
		return "", fmt.Errorf("no packer-plugin-%s_v%s_x%s.*_%s_%s binary in the archive", name, version, apiMajor, goos, goarch)
	}
	rc, err := bin.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".plugin-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), rc)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var errNotFound = errors.New("not found")

func (r *Registry) getJSON(ctx context.Context, u string, gitHub bool, v any) error {
	body, err := r.get(ctx, u, gitHub)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %w", u, err)
	}
	return nil
}

func (r *Registry) get(ctx context.Context, u string, gitHub bool) ([]byte, error) {
	resp, err := r.do(ctx, u, gitHub)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// do sends a GET request and fails on a status other than 200 OK. The
// token goes to the GitHub API only.
func (r *Registry) do(ctx context.Context, u string, gitHub bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if gitHub {
		req.Header.Set("Accept", "application/vnd.github+json")
		if r.Token != "" {
			req.Header.Set("Authorization", "Bearer "+r.Token)
		}
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s: %w", u, errNotFound)
	}
	if gitHub && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if r.Token == "" {
			return nil, errors.New("github: API rate limit exceeded: set GITHUB_TOKEN to raise it, or wait a minute")
		}
		return nil, errors.New("github: API rate limit exceeded, wait a minute")
	}
	var msg struct {
		Message string `json:"message"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(b, &msg) == nil && msg.Message != "" {
		return nil, fmt.Errorf("%s: %s: %s", u, resp.Status, msg.Message)
	}
	return nil, fmt.Errorf("%s: %s", u, resp.Status)
}
