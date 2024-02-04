package main

import (
	"strings"
)

func main() {
	config, err := ParseConfig()
	if err != nil {
		defaultLogger.Error("run failed", "error", err)
		return
	}

	switch strings.ToUpper(config.Role) {
	case "SERVER":
		s := NewServer(config, defaultLogger)
		err = s.Run()
	case "CLIENT":
		c := NewClient(config, defaultLogger)
		err = c.Run()
	default:
		defaultLogger.Error("unknown role in config", "role", config.Role)
	}
	if err != nil {
		defaultLogger.Error("start error", "role", config.Role, "error", err)
		return
	}
}
