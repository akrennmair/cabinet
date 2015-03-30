# cabinet
cabinet is an HTTP server to keep and serve your static files.

It stores the files in a key-value database, and supports a simple replication
scheme.

## Usage

Build and install normally using `go install`.

Start with `cabinet -pass=$PASSWORD -frontend=http://publicaddress:port`. The 
password is necessary for the upload API. The default username is `admin`, but 
can be configured differently.

The `cup` subdirectory contains an example how to use the upload API. The 
frontend address is required for generating complete URLs in the upload API.

To delete files, the same URL as was returned by the upload API needs to be 
called with the HTTP `DELETE` method and authentication like the upload API.

## Replication

cabinet implements a replication scheme. By default, a cabinet instance acts as 
a `parent`, which means it allows uploads and deletions.

A cabinet `child` can be started by providing the commandline option 
`-parent=http://parentserver:port`. A `child` will not allow uploads or 
deletions, but will instead replicate all uploads and deletions as they happen 
from the `parent` instance. `child` instances can be cascaded, i.e. one `child` 
can replicate from another `child`.

Whenever a `child` gets disconnected from its `parent`, it attempts to 
automatically reconnect and catch up with any uploads or deletions that 
happened during the disconnect time.

To enable multi-master replication, you need to start 2 or more instances that
point to each other as parent in the form of a ring, and also set the 
`-forceparent` option. This option enables uploads and deletions in instances 
that would otherwise act as child instances. In such a setup, when uploading a 
file, it is replicated through the whole ring, until the event reaches the 
server where the original upload event was triggered. Since the event is 
already locally available, it is ignored and not distributed any further.
