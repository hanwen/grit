'grit' = 'git' but new and unpolished.


TL;DR
-----

* A writable FUSE filesystem that stores data in Git

* A command 'grit' that lets you do version control in a Grit tree

* Grit supports submodules natively

* Grit lazily fetches blob contents using git protocol v2

* Grit does not expose the .git directory, ensuring that users cannot break abstraction boundaries

* Grit needs go-fuse; probably doesn't work on OSX


BACKGROUND
----------

This is an exploration of how the JJ workflow/data model could work
with Git projects that are large and use submodules. It lacks
functionality for practical use. See TODO file for missing bits.


DESIGN
------

* All of the logic is inside the FUSE daemon; the command-line client
  RPCs to the FUSE daemon

* The git repo serves as storage

* Each workspace is stored in a refs/grit/$WORKSPACE-NAME ref


EXAMPLE
-------
See [example session](docs/example.md).


PERFORMANCE
-----------
See [performance numbers](docs/performance.md).


DISCLAIMER
----------

Prototype; not an official Google product.
