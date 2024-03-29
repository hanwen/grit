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

package gritfs

import (
	"time"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hanwen/go-fuse/v2/fs"
)

// SnapshotResult is the result for the public RepoNode.Snapshot method.
type SnapshotResult struct {
	Recomputed int
	CheckedOut *object.Commit
	State      *WorkspaceState
}

// Properties of a checked out commit
type WorkspaceState struct {
	AutoSnapshot bool
}

// Input to calculating a Snapshot
type WorkspaceUpdate struct {
	// Must be acquired as early as possible to avoid loosing
	// updates in a next run of Snapshot
	TS       time.Time
	Message  string
	Amend    bool
	NewState WorkspaceState
}

type Node interface {
	fs.InodeEmbedder

	snapshot(*WorkspaceUpdate) (snapshotResult, error)

	// Return the last calculated Hash. Does not trigger a new snapshot.
	ID() plumbing.Hash
	DirMode() filemode.FileMode
	GetRepoNode() *RepoNode
	GetTreeNode() *TreeNode
	FitsMode(filemode.FileMode) bool
	setID(plumbing.Hash, filemode.FileMode, *setIDState) error
}

// snapshotResult is the result for the internal snapshot method, computing a SHA1.
type snapshotResult struct {
	// Number of hashes recomputed. Used for verifying incremental updates
	Recomputed int

	// TS for the hash computed below.
	TS   time.Time
	Hash plumbing.Hash
}

type submoduleState struct {
	node   *RepoNode
	parent *fs.Inode
	name   string
	hash   plumbing.Hash

	config *config.Submodule
	err    error
}

type setIDState struct {
	// submodules
	submodules map[string]*submoduleState

	missingSizes []*BlobNode
	ts           time.Time
}
