package cli

// laravelHeaders are the rate-limit headers of Laravel's throttle
// middleware: the remaining count depends on how many requests came before
// in the window (several in parallel during a capture), and the reset time
// and Retry-After on the clock. A request sent past the limit (repeat: in
// the catalog) still compares its status, body and limit header.
const laravelHeaders = `  - name: x-ratelimit-remaining
    format: '\d+'
    reason: Laravel throttle; depends on the requests sent before in the same minute.
  - name: x-ratelimit-reset
    format: '\d{10}'
    reason: Laravel throttle; the time the limit resets (Unix seconds).
  - name: retry-after
    format: '\d+'
    reason: Laravel throttle; seconds until the limit resets.
`
