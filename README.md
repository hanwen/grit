'grit' = 'git' but new and unpolished.


TL;DR
-----

* A writable FUSE filesystem that stores data in Git

* A command 'grit' that lets you do version control in a Grit tree

* Grit supports submodules natively

* Grit does not expose the .git directory, ensuring that users cannot break abstraction boundaries

* Grit needs go-fuse; it probably doesn't work on OSX


EXAMPLE
-------
See [example session](docs/example.md).


PERFORMANCE
-----------
See [performance numbers](docs/performance.md).

