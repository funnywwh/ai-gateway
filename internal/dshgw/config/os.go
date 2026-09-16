package config

import "os"

func openOSFile(path string) (*os.File, error) { return os.Open(path) }
