package appliance

// ReadBootstrapOwnedBy is ReadBootstrap for a file owned by uid, so that a
// test that does not run as root can read one.
func ReadBootstrapOwnedBy(path string, uid int) (Bootstrap, error) { return readBootstrap(path, uid) }
