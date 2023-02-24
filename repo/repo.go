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
	"github.com/hanwen/gritfs/protov2"
)

var noSubmodules = &config.Modules{}

type Repository struct {
	*git.Repository
	repoPath  string
	repoURL   *url.URL
	gitClient *protov2.Client

	mu    sync.Mutex
	sizes map[plumbing.Hash]uint64
}

func (r *Repository) String() string {
	return r.repoURL.String()
}

func (r *Repository) SetDebug(dbg bool) {
	r.gitClient.Debug = true
}

func (r *Repository) CachedBlobSize(id plumbing.Hash) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sz, ok := r.sizes[id]
	return sz, ok
}

func (r *Repository) SubmoduleConfig(commit *object.Commit) (*config.Modules, error) {
	// Can't use commit.FindFile(). .gitmodules might be large,
	// and must be faulted in by calling our BlobObject()
	// implementation
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}

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

func (r *Repository) OpenSubmodule(submod *config.Submodule) (*Repository, error) {
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

	subURL, err := r.repoURL.Parse(submod.URL)
	if err != nil {
		return nil, err
	}
	sr, err := NewRepo(subRepo, repoPath, subURL)
	if err != nil {
		return nil, err
	}
	return sr, nil
}

type submoduleLoad struct {
	r      *Repository
	hash   plumbing.Hash
	commit *object.Commit
	err    error
}

func (r *Repository) loadSubmoduleWorker(todo <-chan *submoduleLoad) error {
	var wg sync.WaitGroup
	var acc []*submoduleLoad
	for t := range todo {
		wg.Add(1)
		acc = append(acc, t)
		go func(sl *submoduleLoad) {
			defer wg.Done()
			// TODO - gs.com returns 500 when asking nonexistent hash.
			sl.commit, sl.err = sl.r.FetchCommit(sl.hash)
		}(t)
	}
	wg.Wait()

	for _, t := range acc {
		if t.err != nil {
			return t.err
		}
	}
	return nil
}

func (r *Repository) recursiveFetch(commit *object.Commit, mods *config.Modules, todo chan<- *submoduleLoad, newBlobs map[plumbing.Hash]int64) error {
	tree, err := r.TreeObject(commit.TreeHash)
	if err != nil {
		return err
	}
	tw := object.NewTreeWalker(tree, true, map[plumbing.Hash]bool{})
	defer tw.Close()

path:
	for {
		path, entry, err := tw.Next()
		if err == storer.ErrStop || err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("next: %v", err)
			return err
		}
		switch entry.Mode {
		case filemode.Regular, filemode.Executable, filemode.Symlink:
			if _, ok := r.sizes[entry.Hash]; !ok {
				obj, err := r.Repository.BlobObject(entry.Hash)
				if err == plumbing.ErrObjectNotFound {
					newBlobs[entry.Hash] = -1
				} else {
					newBlobs[entry.Hash] = obj.Size
				}
			}
			continue path
		case filemode.Submodule:
			break
		case filemode.Dir:
			continue path
		default:
			return fmt.Errorf("unknown mode in %v", entry)
		}

		submod := SubmoduleByPath(mods, path)
		if submod == nil {
			return fmt.Errorf("submodule %q unknown", path)
		}

		subRepo, err := r.OpenSubmodule(submod)
		todo <- &submoduleLoad{
			r:    subRepo,
			hash: entry.Hash,
		}
	}

	return nil
}

func (r *Repository) maybeFetchCommit(id plumbing.Hash) (*object.Commit, error) {
	commit, err := r.CommitObject(id)
	if err == nil {
		return commit, err
	}

	if err != plumbing.ErrObjectNotFound {
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

	if err = r.gitClient.Fetch(r.Repository.Storer, opts); err != nil {
		return nil, err
	}

	return r.CommitObject(id)
}

// FetchCommit fetches the commit, fetching submodules recursively. On
// success all blob sizes are known.
func (r *Repository) FetchCommit(commitID plumbing.Hash) (commit *object.Commit, err error) {
	commit, err = r.maybeFetchCommit(commitID)
	if err != nil {
		return nil, err
	}

	mods, err := r.SubmoduleConfig(commit)
	if err != nil {
		return nil, err
	}

	newBlobs := map[plumbing.Hash]int64{}
	submoduleLoads := make(chan *submoduleLoad, len(mods.Submodules))
	go func() {
		err = r.recursiveFetch(commit, mods, submoduleLoads, newBlobs)
		close(submoduleLoads)
	}()

	if err := r.loadSubmoduleWorker(submoduleLoads); err != nil {
		return nil, err
	}

	if err != nil {
		return nil, err
	}
	if err := r.fetchSizes(newBlobs); err != nil {
		return nil, err
	}
	return commit, nil
}

func (r *Repository) BlobObject(id plumbing.Hash) (*object.Blob, error) {
	obj, err := r.Repository.BlobObject(id)
	if err == plumbing.ErrObjectNotFound {
		opts := &protov2.FetchOptions{
			Progress: os.Stderr,
			Want:     []plumbing.Hash{id},
		}
		if err := r.gitClient.Fetch(r.Storer, opts); err != nil {
			return nil, err
		}

		obj, err = r.Repository.BlobObject(id)
	}

	return obj, err
}

func NewRepo(
	r *git.Repository,
	repoPath string,
	repoURL *url.URL) (*Repository, error) {
	repoPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, err
	}
	cl, err := protov2.NewClient(repoURL.String())
	if err != nil {
		return nil, err
	}
	fn := filepath.Join(repoPath, "blob-sizes.txt")
	sizes, err := readSizes(fn)
	if err != nil {
		return nil, err
	}

	return &Repository{
		Repository: r,
		repoPath:   repoPath,
		repoURL:    repoURL,
		gitClient:  cl,
		sizes:      sizes,
	}, nil
}

func (r *Repository) fetchSizes(newBlobs map[plumbing.Hash]int64) error {
	if len(newBlobs) == 0 {
		return nil
	}
	var keys []plumbing.Hash
	for k, v := range newBlobs {
		if v < 0 {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		log.Printf("Fetching %d sizes for %s", len(keys), r.repoURL)
	}
	newSizes, err := r.gitClient.ObjectInfo(keys)
	if err != nil {
		return err
	}
	for k, v := range newBlobs {
		if v >= 0 {
			newSizes[k] = uint64(v)
		}
	}
	fn := filepath.Join(r.repoPath, "blob-sizes.txt")
	if err := saveSizes(fn, newSizes); err != nil {
		return err
	}

	for k, v := range newSizes {
		r.sizes[k] = v
	}

	return nil
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
