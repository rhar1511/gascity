package citywriteauth

// MaxHTTPBodyBytes is the common HTTP request-envelope budget, including JSON
// quoting and base64 overhead. It is not a limit on standalone retained files.
// Clients must size the serialized UTF-8 body before signing its request digest.
const MaxHTTPBodyBytes = 1 << 20
