# Third-party content in these records

The build records in this folder (`*/provenance.json`, `*/logs.bin`) are
builds of the Dockerfile of moby/moby (https://github.com/moby/moby, tag
docker-v29.9.2). Each provenance carries that Dockerfile in full,
base64-encoded. For the tests, the copy built was changed: an
`ENV CI_ACCELERATOR_CHECK` line in its base stage, and one-line edits of
files of the build context (see `moby_test.go`).

moby/moby is licensed under the Apache License, Version 2.0
(https://www.apache.org/licenses/LICENSE-2.0, the same licence as this
repository, see `LICENSE` at its root). Its NOTICE reads, in the part
that concerns it:

    Copyright 2012-2017 Docker, Inc.

    This product includes software developed at Docker, Inc. (https://www.docker.com).

These files are test data only: they are not part of the binary, the
image or the charts.
