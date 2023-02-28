
Performance
===========

Performance testing uses the Android AOSP checkout, which is available
without requiring auth.

Timings on Lenovo T460 (i5-6440HQ CPU @ 2.60GHz 4-core CPU.)


Command to start FUSE
---------------------

```
git init --bare ~/tmp/android
mkdir /tmp/x
fusermount -u /tmp/x ; go build ./cmd/fuse ; echo running; ./fuse --url https://android.googlesource.com/platform/superproject -workspace ws3 -repo ~/tmp/android/ /tmp/x
```

Commands to test
----------------

Build client binary:
```
go build ./cmd/grit
```

Fetch files for recent Android commit, filter at 10kb. (residential
300 mbps connection, in Munich)

```
./grit -dir /tmp/x/ checkout 0f179a7b7277ca56a2e3aade6bafdf0527e40361

3m23.943911849s
```

Visit all file nodes in-memory, sequentially:

```
$ time ./grit -dir /tmp/x/ visit
real	0m0.384s
```

Visit all file nodes, print filenames
```
$ time ./grit -dir /tmp/x/ find -type f | wc
1056662 1057257 85562080

real	1m1.018s
```

no-op snapshot
```
$ time ./grit -dir /tmp/x/ snapshot
Recomputed 0 hashes

real	0m0.549s
```

snapshot after file modification:

```
$ echo hello > /tmp/x/frameworks/base/docs/test.md
$ time ./grit -dir /tmp/x/ snapshot
Recomputed 6 hashes

real	0m0.345s
user	0m0.008s
sys	0m0.008s
```

discard tree:

```
$ time ./grit -dir /tmp/x/ wslog
2023-03-01 22:54:45 at commit 635545825d3427a7191c913c5de697655b4d5255 - Snapshot created 01 Mar 23 22:54 +0100 for tree 33a77646abf7e6b36cb9221cad381624148ff70f
  Reason: snapshot command
  Metadata ID: 50e88b9620195fdc
  Status: gritfs.WorkspaceState{AutoSnapshot:true}
2023-03-01 22:50:21 at commit 0f179a7b7277ca56a2e3aade6bafdf0527e40361 - Merge "[cbor] Refactor building the first part of bcc with ciborium"
  Reason: SetID
  Metadata ID: 1a4976f2b55849b9
  Status: gritfs.WorkspaceState{AutoSnapshot:false}
2023-03-01 22:45:27 at commit 66607743f4c3e979a5b7fd1438224ef6eb459338 - initial commit
  Reason: initialize workspace
  Metadata ID: 6d7c6f4637213b23
  Status: gritfs.WorkspaceState{AutoSnapshot:false}

real	0m0.037s

$ time ./grit -dir /tmp/x/ checkout 66607743f4c3e979a5b7fd1438224ef6eb459338

real	0m0.026s
```

repopulate tree at commit locally available.

```
$ time ./grit -dir /tmp/x/ checkout 0f179a7b7277ca56a2e3aade6bafdf0527e40361

real	0m2.734s
```
