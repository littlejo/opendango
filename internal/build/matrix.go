package build

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// Variant is one build of a matrix: the plan of the Dangofile with one
// value of each matrix variable.
type Variant struct {
	// Name tells the variant apart in the log and the errors
	// (NAME=value ...).
	Name string
	// Values are the matrix variables, the custom_data of its manifest
	// entries.
	Values map[string]string
	Plan   *plan.Plan
}

// RunMatrix builds the variants at the same time, each like Run (opts.Jobs
// stages at a time per variant), like the sources of a Packer build block.
// They share the terminal, the manifests (one run UUID) and the caches: a
// stage with the same cache key in several variants is built once, the
// others wait for it and find it in the cache. A failed variant does not
// stop the others. Two variants that would write the same OUTPUT or FETCH
// file, or the same named template, are an error before anything starts.
func RunMatrix(ctx context.Context, variants []Variant, opts Options) error {
	if err := checkMatrix(variants, opts); err != nil {
		return err
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	out := &lockedWriter{w: opts.Out}
	opts.manifests = newManifests(variants[0].Plan.Builtins[plan.BuildIDVar])
	if opts.Stdin != nil {
		opts.console = &console{in: bufio.NewReader(opts.Stdin)}
	}
	opts.building = &keyLocks{}
	errs := make([]error, len(variants))
	var wg sync.WaitGroup
	for i, v := range variants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newPrefixWriter(out, "["+v.Name+"] ")
			defer w.Flush()
			vopts := opts
			vopts.Out = w
			vopts.matrix = v.Values
			if err := Run(ctx, v.Plan, vopts); err != nil {
				errs[i] = fmt.Errorf("[%s] %w", v.Name, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// checkMatrix refuses variants that would write the same file or template:
// the matrix variables must make the names differ. The manifests are shared
// (each variant adds its builds).
func checkMatrix(variants []Variant, opts Options) error {
	written := map[string]string{}
	var errs []error
	claim := func(v Variant, what string) {
		if prev, ok := written[what]; ok && prev != v.Name {
			errs = append(errs, fmt.Errorf("[%s] and [%s] both write %s: use a matrix variable in it", prev, v.Name, what))
			return
		}
		written[what] = v.Name
	}
	path := func(p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(opts.ContextDir, p)
		}
		return filepath.Clean(p)
	}
	for _, v := range variants {
		stages, err := v.Plan.Select(opts.Target)
		if err != nil {
			return fmt.Errorf("[%s] %w", v.Name, err)
		}
		for _, s := range stages {
			for _, o := range s.Outputs {
				switch {
				case o.Kind == instructions.OutputTemplate && o.Name != "":
					claim(v, "the template "+o.Name)
				case o.Kind == instructions.OutputManifest:
				case o.Path != "":
					claim(v, path(o.Path))
				}
			}
			for _, step := range s.Steps {
				if f, ok := step.(*instructions.Fetch); ok {
					claim(v, path(f.Dest))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// keyLocks lets one stage per cache key run at a time. The zero value and a
// nil *keyLocks work; a nil one never waits.
type keyLocks struct {
	mu   sync.Mutex
	busy map[string]chan struct{}
}

// lock waits until no stage of key runs (calling wait once when it has to
// wait), then holds key until release. An empty key is not locked.
func (l *keyLocks) lock(ctx context.Context, key string, wait func()) (release func(), err error) {
	if l == nil || key == "" {
		return func() {}, nil
	}
	waited := false
	for {
		l.mu.Lock()
		ch, ok := l.busy[key]
		if !ok {
			if l.busy == nil {
				l.busy = map[string]chan struct{}{}
			}
			ch = make(chan struct{})
			l.busy[key] = ch
			l.mu.Unlock()
			return func() {
				l.mu.Lock()
				delete(l.busy, key)
				l.mu.Unlock()
				close(ch)
			}, nil
		}
		l.mu.Unlock()
		if !waited {
			waited = true
			wait()
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
