EXAMPLE
=======

To start the daemon,

```
git init --bare ~/tmp/gerrit
mkdir /tmp/x
go build ./cmd/fuse/
./fuse -url https://gerrit.googlesource.com/gerrit -repo ~/tmp/gerrit  /tmp/x
```

Example session:

The `workspace` command manages workspaces

```
./grit -dir /tmp/x/ workspace create ws1
./grit -dir /tmp/x/ workspace list
ws1
```

`checkout` fetches a revisions and populates the tree.
```
./grit -dir /tmp/x/ws1 checkout 4f326673affe9172ca1c76ffd15af6d120855164

$ ls /tmp/x/ws1/
antlr3  contrib  Documentation  INSTALL  javatests    lib      package.json  polygerrit-ui    prolog       proto      resources           tools        webapp                     WORKSPACE
BUILD   COPYING  e2e-tests      java     Jenkinsfile  modules  plugins       polymer-bridges  prologtests  README.md  SUBMITTING_PATCHES  version.bzl  web-dev-server.config.mjs  yarn.lock
```

Submodules are populated recursively,

```
[hanwen@localhost grit]$ head /tmp/x/ws1/modules/jgit/README.md
# Java Git

An implementation of the Git version control system in pure Java.
```

Within the checkout, Git data for submodules is available separately:

```
$ cd /tmp/x/ws1
$ grit log
commit 4f326673affe9172ca1c76ffd15af6d120855164
Author: Dhruv Srivastava <dhruvsri@google.com>
Date:   Wed Mar 01 15:22:34 2023 +0100

    Move AuthInit from service to types.ts

[hanwen@localhost x]$ cd modules/jgit/
[hanwen@localhost jgit]$ grit log
commit 176f17d05ec154ce455ab2bde7429017d43d67fb
Author: Matthias Sohn <matthias.sohn@sap.com>
Date:   Wed Feb 22 21:06:41 2023 +0100

    Merge branch 'stable-6.4'
```

The checkout is writable, and snapshots are taken automatically:

```
[hanwen@localhost jgit]$ echo hello  >> README.md
[hanwen@localhost jgit]$ grit log -p
commit 70cbbfd2adf13bd6741a67c14aa52127f3fc746f
Author: Han-Wen Nienhuys <hanwen@localhost>
Date:   Wed Mar 01 23:18:05 2023 +0100

    Snapshot created 01 Mar 23 23:18 +0100 for tree be4b682c9fd643ba042ce11748f047bdadb72bb9


diff --git a/README.md b/README.md
index f1f485adcae6286fc9268ebf00a0bf82bba24c0c..1ba24f528f4276f093631029748e7135eca0832c 100644
--- a/README.md
+++ b/README.md
@@ -177,3 +177,4 @@
 More information about Git, its repository format, and the canonical
 C based implementation can be obtained from the
 [Git website](http://git-scm.com/).
+hello
```

With the `commit` command, you can add a message
```
[hanwen@localhost jgit]$ grit commit -m 'add a greeting'
[hanwen@localhost jgit]$ grit log -p
commit f619ee611639ee9792a5045beb283d0624cc26fc
Author: Han-Wen Nienhuys <hanwen@localhost>
Date:   Wed Mar 01 23:18:05 2023 +0100

    add a greeting


diff --git a/README.md b/README.md
index f1f485adcae6286fc9268ebf00a0bf82bba24c0c..1ba24f528f4276f093631029748e7135eca0832c 100644
--- a/README.md
+++ b/README.md
@@ -177,3 +177,4 @@
 More information about Git, its repository format, and the canonical
 C based implementation can be obtained from the
 [Git website](http://git-scm.com/).
+hello
```

Changes to the "workspace" are recorded in `refs/grit/WORKSPACE-NAME` and can be queried with `wslog`

```
[hanwen@localhost jgit]$ grit wslog
2023-03-01 23:18:43 at commit f619ee611639ee9792a5045beb283d0624cc26fc - add a greeting
  Reason: commit
  Metadata ID: 3e4440011bd83c83
  Status: gritfs.WorkspaceState{AutoSnapshot:false}
2023-03-01 23:18:05 at commit 70cbbfd2adf13bd6741a67c14aa52127f3fc746f - Snapshot created 01 Mar 23 23:18 +0100 for tree be4b682c9fd643ba042ce11748f047bdadb72bb9
  Reason: log call
  Metadata ID: 2e53c5d88f4065f7
  Status: gritfs.WorkspaceState{AutoSnapshot:true}
2023-03-01 23:12:24 at commit 176f17d05ec154ce455ab2bde7429017d43d67fb - Merge branch 'stable-6.4'
  Reason: SetID
  Metadata ID: 60f9842dea3bff0b
  Status: gritfs.WorkspaceState{AutoSnapshot:false}
2023-03-01 23:12:24 at commit 421686c08716295872985dcaebec35b767727feb - initial commit
  Reason: initialize workspace
  Metadata ID: 66c1cda95fdafe10
  Status: gritfs.WorkspaceState{AutoSnapshot:false}
```

Each git repository has its own workspace log,
```
[hanwen@localhost jgit]$ cd ../..
[hanwen@localhost x]$ pwd
/tmp/x
[hanwen@localhost x]$ grit wslog
2023-03-01 23:12:25 at commit 4f326673affe9172ca1c76ffd15af6d120855164 - Move AuthInit from service to types.ts
  Reason: SetID
  Metadata ID: 0d087e681535ecbe
  Status: gritfs.WorkspaceState{AutoSnapshot:false}
2023-03-01 23:12:13 at commit 9e07fa03d74b30c15d4fbe4d83e7ffebab4e2200 - initial commit
  Reason: initialize workspace
  Metadata ID: abf097d60c3540cb
  Status: gritfs.WorkspaceState{AutoSnapshot:false}
```

You can request an explicit snapshot

```
[hanwen@localhost x]$ grit snapshot
Recomputed 3 hashes
[hanwen@localhost x]$ grit wslog
2023-03-01 23:24:25 at commit 860691cccbd9be777d16986cc65634d5fc916151 - Snapshot created 01 Mar 23 23:24 +0100 for tree f24bbeffa805499edc06d0955579b8b5e97f4f55
  Reason: snapshot command
  Metadata ID: fe44bf4f41cc61f0
  Status: gritfs.WorkspaceState{AutoSnapshot:true}
2023-03-01 23:12:25 at commit 4f326673affe9172ca1c76ffd15af6d120855164 - Move AuthInit from service to types.ts
  Reason: SetID
  Metadata ID: 0d087e681535ecbe
  Status: gritfs.WorkspaceState{AutoSnapshot:false}
2023-03-01 23:12:13 at commit 9e07fa03d74b30c15d4fbe4d83e7ffebab4e2200 - initial commit
  Reason: initialize workspace
  Metadata ID: abf097d60c3540cb
  Status: gritfs.WorkspaceState{AutoSnapshot:false}
```

The change in the submodule triggered a new snapshot:

```
$ git --git-dir ~/tmp/gerrit diff 4f326673affe9172ca1c76ffd15af6d120855164 860691cccbd9be777d16986cc65634d5fc916151 | cat
diff --git a/modules/jgit b/modules/jgit
index 176f17d..f619ee6 160000
--- a/modules/jgit
+++ b/modules/jgit
@@ -1 +1 @@
-Subproject commit 176f17d05ec154ce455ab2bde7429017d43d67fb
+Subproject commit f619ee611639ee9792a5045beb283d0624cc26fc
```




