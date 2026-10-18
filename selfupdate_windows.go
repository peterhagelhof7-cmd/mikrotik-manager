package main

import "errors"

// Unter Windows nur für Entwicklung/Tests übersetzt – dort kein Selbst-Update.
func execSelf(string) error { return errors.New("unter Windows nicht unterstützt") }
