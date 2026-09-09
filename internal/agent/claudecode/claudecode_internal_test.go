package claudecode

import "github.com/agenticworkflowdev/cli/internal/agent"

// StreamReplay is the parsed shape a black-box contract test needs from the
// stdout stream parser.
type StreamReplay struct {
	Progress                []agent.ProgressEvent
	SessionID               string
	ResultPresent           bool
	ResultIsError           bool
	StructuredOutputPresent bool
	StructuredOutput        []byte
	APIErrorStatus          string
}

// ReplayStream feeds recorded stream-json bytes through the exact production
// parser so contract fixtures are verified against real adapter behaviour.
func ReplayStream(contents []byte) (StreamReplay, error) {
	stream, err := parseStream(contents, nil)
	if err != nil {
		return StreamReplay{}, err
	}
	replay := StreamReplay{Progress: stream.progress, SessionID: stream.sessionID}
	if stream.result != nil {
		replay.ResultPresent = true
		replay.ResultIsError = stream.result.isError
		replay.APIErrorStatus = stream.result.apiErrorStatus
		replay.StructuredOutputPresent = len(stream.result.structuredOutput) > 0
		replay.StructuredOutput = stream.result.structuredOutput
	}
	return replay, nil
}
