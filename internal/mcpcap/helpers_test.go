package mcpcap_test

import "os"

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }
