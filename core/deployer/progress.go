package deployer

// ProgressReporter publishes stable machine-readable stages and concise
// operator-facing messages. Reporters must return quickly; deployment work is
// blocked while a report is persisted.
type ProgressReporter func(stage, message string, progress int)

func reportProgress(reporter ProgressReporter, stage, message string, progress int) {
	if reporter != nil {
		reporter(stage, message, progress)
	}
}
