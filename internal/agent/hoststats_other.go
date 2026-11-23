//go:build !unix

package agent

func diskSpace(string) (free, total int64) { return 0, 0 }
