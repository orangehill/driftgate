package httpx

import "syscall"

// resetErrors are the system's errors of a connection the server reset:
// Windows names them by their WSA codes, which syscall.ECONNRESET is not.
var resetErrors = []error{syscall.WSAECONNRESET, syscall.WSAECONNABORTED}
