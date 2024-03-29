// Copyright 2023 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repo

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/hanwen/gritfs/gitutil"
	"github.com/hanwen/gritfs/protov2"
)

var noSubmodules = &config.Modules{}

// Repository is a thread-safe wrapper around git.Repository
type Repository struct {
	repoPath  string
	repoURL   *url.URL
	gitClient *protov2.Client

	mu         sync.Mutex
	sizes      map[plumbing.Hash]uint64
	repo       *git.Repository
	submodules map[string]*Repository
}

func (r *Repository) String() string {
	return r.repoURL.String()
}

func (r *Repository) SetDebug(dbg bool) {
	r.gitClient.Debug = true
}

func (r *Repository) References() (storer.ReferenceIter, error) {
	return r.repo.References()
}

func (r *Repository) RemoveReference(nm plumbing.ReferenceName) error {
	return r.repo.Storer.RemoveReference(nm)
}

func (r *Repository) CachedBlobSize(id plumbing.Hash) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sz, ok := r.sizes[id]
	return sz, ok
}

func (r *Repository) SaveBytes(b []byte) (id plumbing.Hash, err error) {
	return r.SaveBlob(bytes.NewBuffer(b))
}

func (r *Repository) SaveBlob(rd io.Reader) (id plumbing.Hash, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	enc := r.repo.Storer.NewEncodedObject()
	enc.SetType(plumbing.BlobObject)
	w, err := enc.Writer()
	if err != nil {
		return id, err
	}

	sz, err := io.Copy(w, rd)
	if err != nil {
		return id, err
	}
	if err := w.Close(); err != nil {
		return id, err
	}

	id, err = r.repo.Storer.SetEncodedObject(enc)
	if err != nil {
		return id, err
	}

	if err := r.saveSizes(map[plumbing.Hash]uint64{id: uint64(sz)}); err != nil {
		return id, err
	}
	r.sizes[id] = uint64(sz)
	return id, nil
}

func (r *Repository) SubmoduleConfigByCommit(commit *object.Commit) (*config.Modules, error) {
	// Can't use commit.FindFile(). .gitmodules might be large,
	// and must be faulted in by calling our BlobObject()
	// implementation
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	return r.SubmoduleConfig(tree)
}

func (r *Repository) SubmoduleConfig(tree *object.Tree) (*config.Modules, error) {

	entry, err := tree.FindEntry(".gitmodules")
	if err == object.ErrEntryNotFound {
		return noSubmodules, nil
	}
	if err != nil {
		return nil, err
	}

	blob, err := r.BlobObject(entry.Hash)
	if err != nil {
		return nil, err
	}

	rd, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	data, err := ioutil.ReadAll(rd)
	if err != nil {
		return nil, err
	}

	mods := config.NewModules()
	if err := mods.Unmarshal(data); err != nil {
		return nil, err
	}
	return mods, nil
}

func SubmoduleByPath(mods *config.Modules, path string) *config.Submodule {
	for _, m := range mods.Submodules {
		if m.Path == path {
			return m
		}
	}
	return nil
}

func (r *Repository) DiffRecursiveByCommit(from, to *object.Commit) (object.Changes, error) {
	t1, err := from.Tree()
	if err != nil {
		return nil, err
	}
	t2, err := to.Tree()
	if err != nil {
		return nil, err
	}

	return r.DiffRecursive(t1, t2)
}

func (r *Repository) DiffRecursive(from, to *object.Tree) (object.Changes, error) {
	var result []*object.Change

	chs, err := object.DiffTree(from, to)
	if err != nil {
		return nil, err
	}

	cfg, err := r.SubmoduleConfig(to)
	if err != nil {
		return nil, err
	}

	for _, ch := range chs {
		if ch.From.TreeEntry.Mode != ch.To.TreeEntry.Mode && (ch.To.TreeEntry.Mode == filemode.Submodule ||
			ch.From.TreeEntry.Mode == filemode.Submodule) {
			return nil, fmt.Errorf("%q=>%q mixed submodule change not supported", ch.From.Name, ch.To.Name)
		}

		if ch.To.TreeEntry.Mode != filemode.Submodule {
			result = append(result, ch)
			continue
		}
		sm := SubmoduleByPath(cfg, ch.To.Name)
		if sm == nil {
			return nil, fmt.Errorf("no submodule for %q", ch.To.Name)
		}

		subRepo, err := r.OpenSubmodule(sm)
		if err != nil {
			return nil, err
		}

		h1 := ch.From.TreeEntry.Hash
		h2 := ch.To.TreeEntry.Hash

		c1, err := subRepo.CommitObject(h1)
		if err != nil {
			return nil, err
		}
		c2, err := subRepo.CommitObject(h2)
		if err != nil {
			return nil, err
		}

		t1, err := c1.Tree()
		if err != nil {
			return nil, err
		}
		t2, err := c2.Tree()
		if err != nil {
			return nil, err
		}

		subChs, err := subRepo.DiffRecursive(t1, t2)
		if err != nil {
			return nil, err
		}

		for _, subCh := range subChs {
			// Can't tweak subCh.{from,to}.name ; the name
			// is used to form patch contents.
			result = append(result, subCh)
		}
	}

	return result, nil
}

func (r *Repository) OpenSubmodule(submod *config.Submodule) (*Repository, error) {
	subURL, err := r.repoURL.Parse(submod.URL)
	if err != nil {
		return nil, err
	}

	{
		r.mu.Lock()
		sr, ok := r.submodules[subURL.String()]
		r.mu.Unlock()

		if ok {
			return sr, nil
		}
	}
	repoPath := filepath.Join(r.repoPath, "modules", submod.Name)
	subRepo, err := git.PlainOpen(repoPath)
	if err != nil {
		repoPath = filepath.Join(r.repoPath, "modules", strings.Replace(submod.Name, "/", "%2f", -1))
		subRepo, err = git.PlainOpen(repoPath)
	}
	if err == git.ErrRepositoryNotExists {
		subRepo, err = git.PlainInit(repoPath, true)
	}
	if err != nil {
		log.Printf("opening/creating %q: %v", repoPath, err)
		return nil, err
	}

	sr, err := NewRepo(subRepo, repoPath, subURL)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.submodules[subURL.String()] = sr
	return sr, nil
}

func (r *Repository) maybeFetchCommit(id plumbing.Hash) (*object.Commit, error) {
	commit, err := r.CommitObject(id)
	if err == nil {
		return commit, err
	}

	if err != plumbing.ErrObjectNotFound {
		return nil, err
	}
	if err := r.setClient(); err != nil {
		return nil, err
	}

	opts := &protov2.FetchOptions{
		Progress: os.Stderr,
		Depth:    1,
		Filter:   "blob:limit=10240",
		Want:     []plumbing.Hash{id},
	}

	if !r.gitClient.HasCap("object-info") {
		opts.Filter = ""
	}

	if err = r.gitClient.Fetch(r.repo.Storer, opts); err != nil {
		return nil, err
	}

	return r.CommitObject(id)
}

func (r *Repository) fetchSubmoduleCommit(submod *config.Submodule, id plumbing.Hash) error {
	subRepo, err := r.OpenSubmodule(submod)
	if err != nil {
		return fmt.Errorf("OpenSubmodule(%q): %v", submod.Name, err)
	}

	_, err = subRepo.FetchCommit(id)
	return err
}

// FetchCommit fetches the commit, fetching submodules recursively
func (r *Repository) FetchCommit(commitID plumbing.Hash) (commit *object.Commit, err error) {
	commit, err = r.maybeFetchCommit(commitID)
	if err != nil {
		return nil, fmt.Errorf("maybeFetchCommit: %v", err)
	}

	mods, err := r.SubmoduleConfigByCommit(commit)
	if err != nil {
		return nil, fmt.Errorf("SubmoduleConfig: %v", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("Tree: %v", err)
	}

	errs := make(chan error, len(mods.Submodules))
	count := 0
	for _, submod := range mods.Submodules {
		entry, err := tree.FindEntry(submod.Path)
		if err != nil {
			// it's easy to remove the entry without
			// updating the .gitmodules file
			continue
		}
		if entry.Mode != filemode.Submodule {
			continue
		}

		l := submod

		count++
		go func() {
			var err error
			defer func() { errs <- err }()
			err = r.fetchSubmoduleCommit(l, entry.Hash)
			if err != nil {
				err = fmt.Errorf("fetchSubmoduleCommit(%s): %v", l.Path, err)
			}
		}()
	}

	for i := 0; i < count; i++ {
		err := <-errs
		if err != nil {
			return nil, err
		}
	}

	return commit, nil
}

func (r *Repository) BlobObject(id plumbing.Hash) (*object.Blob, error) {
	obj, err := r.repo.BlobObject(id)
	if err == plumbing.ErrObjectNotFound {
		if err := r.setClient(); err != nil {
			return nil, err
		}
		opts := &protov2.FetchOptions{
			Progress: os.Stderr,
			Want:     []plumbing.Hash{id},
		}
		if err := r.gitClient.Fetch(r.repo.Storer, opts); err != nil {
			return nil, err
		}

		obj, err = r.repo.BlobObject(id)
	}

	return obj, err
}

func (r *Repository) CommitObject(id plumbing.Hash) (*object.Commit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.repo.CommitObject(id)
}

func (r *Repository) SaveCommit(c *object.Commit) (plumbing.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return gitutil.SaveCommit(r.repo.Storer, c)
}

func (r *Repository) TreeObject(id plumbing.Hash) (*object.Tree, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.repo.TreeObject(id)
}

func (r *Repository) PatchTree(tr *object.Tree, se []object.TreeEntry) (plumbing.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return gitutil.PatchTree(r.repo.Storer, tr, se)
}

func (r *Repository) Reference(ref plumbing.ReferenceName, res bool) (*plumbing.Reference, error) {
	return r.repo.Reference(ref, res)
}

func (r *Repository) SetReference(ref *plumbing.Reference) error {
	return r.repo.Storer.SetReference(ref)
}

func (r *Repository) Log(opts *git.LogOptions) (object.CommitIter, error) {
	// todo locking?
	return r.repo.Log(opts)
}

func (r *Repository) SaveTree(se []object.TreeEntry) (plumbing.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return gitutil.SaveTree(r.repo.Storer, se)
}

func NewRepo(
	r *git.Repository,
	repoPath string,
	repoURL *url.URL) (*Repository, error) {
	repoPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, err
	}
	fn := filepath.Join(repoPath, "blob-sizes.txt")
	sizes, err := readSizes(fn)
	if err != nil {
		return nil, err
	}

	return &Repository{
		repo:       r,
		repoPath:   repoPath,
		repoURL:    repoURL,
		sizes:      sizes,
		submodules: map[string]*Repository{},
	}, nil
}

func (r *Repository) setClient() error {
	if r.gitClient != nil {
		return nil
	}
	cl, err := protov2.NewClient(r.repoURL.String())
	if err != nil {
		return err
	}
	r.gitClient = cl
	return nil
}

func (r *Repository) objectSizesFromBlobs(keys []plumbing.Hash) (map[plumbing.Hash]uint64, error) {
	res := map[plumbing.Hash]uint64{}
	for _, k := range keys {
		b, err := r.repo.BlobObject(k)
		if err != nil {
			// todo: should fetch missing blobs
			return nil, err
		}
		res[k] = uint64(b.Size)
	}
	return res, nil
}

func (r *Repository) ObjectSizes(keys []plumbing.Hash) (map[plumbing.Hash]uint64, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	if err := r.setClient(); err != nil {
		return nil, err
	}
	if !r.gitClient.HasCap("object-info") {
		return r.objectSizesFromBlobs(keys)
	}

	log.Printf("Fetching %d sizes for %s", len(keys), r.repoURL)
	newSizes, err := r.gitClient.ObjectInfo(keys)
	if err != nil {
		return nil, err
	}
	if err := r.saveSizes(newSizes); err != nil {
		return nil, err
	}

	for k, v := range newSizes {
		r.sizes[k] = v
	}

	return newSizes, nil
}

func (r *Repository) saveSizes(sizes map[plumbing.Hash]uint64) error {
	fn := filepath.Join(r.repoPath, "blob-sizes.txt")
	return saveSizes(fn, sizes)
}

func saveSizes(filename string, sizes map[plumbing.Hash]uint64) error {
	buf := &bytes.Buffer{}
	for k, v := range sizes {
		fmt.Fprintf(buf, "%v %012d\n", k, v)
	}

	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readSizes(filename string) (map[plumbing.Hash]uint64, error) {
	f, err := os.Open(filename)
	if os.IsNotExist(err) {
		return map[plumbing.Hash]uint64{}, nil
	}

	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Split(bufio.ScanLines)

	result := map[plumbing.Hash]uint64{}
	for s.Scan() {
		str := s.Text()
		h := plumbing.NewHash(str[:40])
		sz, err := strconv.ParseUint(str[41:], 10, 64)
		if err != nil {
			return nil, err
		}
		result[h] = sz
	}

	return result, nil
}
