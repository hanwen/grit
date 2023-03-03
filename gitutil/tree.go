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

package gitutil

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

type sortableEntries []object.TreeEntry

func (sortableEntries) sortName(te object.TreeEntry) string {
	if te.Mode == filemode.Dir {
		return te.Name + "/"
	}
	return te.Name
}
func (se sortableEntries) Len() int               { return len(se) }
func (se sortableEntries) Less(i int, j int) bool { return se.sortName(se[i]) < se.sortName(se[j]) }
func (se sortableEntries) Swap(i int, j int)      { se[i], se[j] = se[j], se[i] }

func SortTreeEntries(es []object.TreeEntry) {
	se := sortableEntries(es)
	sort.Sort(se)
}

type lazyTreeNode struct {
	mode filemode.FileMode

	// for Dir nodes, either id == zero or children != nil
	id       plumbing.Hash
	children map[string]*lazyTreeNode
}

func (n *lazyTreeNode) print(indent int) {
	fmt.Printf("%*sid=%v {\n", indent, "", n.id)
	indent++
	for k, v := range n.children {
		fmt.Printf("%*s%s:\n", indent, "", k)
		v.print(indent)
	}
	indent--
	fmt.Printf("%*s}\n", indent, "")

}
func (n *lazyTreeNode) encode(s storer.EncodedObjectStorer) (plumbing.Hash, error) {
	if n.id != plumbing.ZeroHash {
		return n.id, nil
	}

	if len(n.children) == 0 {
		return plumbing.ZeroHash, nil
	}

	var es []object.TreeEntry
	for k, ch := range n.children {
		id, err := ch.encode(s)
		if err != nil {
			return plumbing.ZeroHash, err
		}

		if id != plumbing.ZeroHash {
			es = append(es, object.TreeEntry{
				Name: k,
				Hash: id,
				Mode: ch.mode,
			})
		}
	}

	return SaveTree(s, es)
}

func (n *lazyTreeNode) materialize(t *object.Tree) {
	if n.mode != filemode.Dir || n.children != nil {
		return
	}

	n.id = plumbing.ZeroHash
	n.children = map[string]*lazyTreeNode{}
	for _, t := range t.Entries {
		n.children[t.Name] = &lazyTreeNode{
			id:   t.Hash,
			mode: t.Mode,
		}
	}
}

func (lt *lazyTreeNode) patch(eos storer.EncodedObjectStorer, entry object.TreeEntry) error {
	path := entry.Name
	sepIdx := strings.Index(path, "/")
	if sepIdx == -1 {
		if entry.Hash == plumbing.ZeroHash {
			delete(lt.children, path)
		} else {
			lt.children[path] = &lazyTreeNode{
				id:   entry.Hash,
				mode: entry.Mode,
			}
		}
	} else {
		dir := path[:sepIdx]
		ch := lt.children[dir]
		if ch == nil || ch.mode != filemode.Dir {
			ch = &lazyTreeNode{
				mode:     filemode.Dir,
				children: map[string]*lazyTreeNode{},
			}
			lt.children[dir] = ch
		} else if ch.id != plumbing.ZeroHash {
			t, err := object.GetTree(eos, ch.id)
			if err != nil {
				return err
			}

			ch.materialize(t)
		}

		entry.Name = path[sepIdx+1:]
		if err := ch.patch(eos, entry); err != nil {
			return err
		}
	}
	return nil
}

// PatchTree constructs a new tree by applying changes to it. In changes,
// the ZeroHash signifies deletion of the path.
func PatchTree(eos storer.EncodedObjectStorer, t *object.Tree, changes []object.TreeEntry) (id plumbing.Hash, err error) {
	root := lazyTreeNode{
		mode: filemode.Dir,
		id:   t.Hash,
	}
	root.materialize(t)

	for _, change := range changes {
		change.Name = filepath.Clean(change.Name)
		if err := root.patch(eos, change); err != nil {
			return id, err
		}
	}

	return root.encode(eos)
}
