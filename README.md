'grit' = 'git' but new and unpolished.


TL;DR
-----

* A writable FUSE filesystem that stores data in Git

* A command 'grit' that lets you do version control in a Grit tree

* Grit supports submodules natively

* Grit does not expose the .git directory, ensuring that users cannot break abstraction boundaries

* Grit needs go-fuse; it probably doesn't work on OSX


VISION
------

By storing all data in Git (and doing away with the worktree and
index), we can provide a filesystem-in-the-cloud that only needs a git
storage service.

By bypassing the Git command-line, we can control the entire user-experience. This opens possibilities such as:

* implementing "hg evolve" workflow
* Implementing the workflow as a web UI
* Specialized Gerrit support

By implementing submodule support natively, we can provide a first-class support for Gerrit topics.


EXAMPLE
-------
See [example session](docs/example.md).
