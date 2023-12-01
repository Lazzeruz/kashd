package main

import (
	"github.com/Kash-Protocol/kashd/infrastructure/logger"
	"github.com/Kash-Protocol/kashd/util/panics"
)

var (
	backendLog = logger.NewBackend()
	log        = backendLog.Logger("MNJS")
	spawn      = panics.GoroutineWrapperFunc(log)
)
