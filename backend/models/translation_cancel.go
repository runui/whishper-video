package models

import (
	"context"
	"sync"
)

var translationCancels sync.Map

func RegisterTranslationCancel(id string, cancel context.CancelFunc) {
	translationCancels.Store(id, cancel)
}

func UnregisterTranslationCancel(id string) {
	translationCancels.Delete(id)
}

func CancelTranslation(id string) {
	if cancel, ok := translationCancels.LoadAndDelete(id); ok {
		cancel.(context.CancelFunc)()
	}
}
