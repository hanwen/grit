package gitutil

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

func SaveBlob(st storer.EncodedObjectStorer, data []byte) (id plumbing.Hash, err error) {
	enc := st.NewEncodedObject()
	enc.SetType(plumbing.BlobObject)
	w, err := enc.Writer()
	if err != nil {
		return
	}
	if _, err := w.Write(data); err != nil {
		return id, err
	}
	if err := w.Close(); err != nil {
		return id, err
	}
	return st.SetEncodedObject(enc)
}

func SaveTree(st storer.EncodedObjectStorer, entries []object.TreeEntry) (id plumbing.Hash, err error) {
	SortTreeEntries(entries)

	enc := st.NewEncodedObject()
	enc.SetType(plumbing.TreeObject)

	t := object.Tree{Entries: entries}
	if err := t.Encode(enc); err != nil {
		return id, err
	}

	return st.SetEncodedObject(enc)
}

func SaveCommit(st storer.EncodedObjectStorer, c *object.Commit) (id plumbing.Hash, err error) {
	enc := st.NewEncodedObject()
	enc.SetType(plumbing.CommitObject)
	if err := c.Encode(enc); err != nil {
		return id, err
	}
	return st.SetEncodedObject(enc)
}
