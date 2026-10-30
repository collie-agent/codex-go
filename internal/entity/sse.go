package entity

type TaskMessage struct {
	Input  string
	TaskId string
	ConvId string
}

type LLMResponse struct {
	Type  string
	Delta string
	Err   error
}

// StreamEvent is one event read from a task response stream.
type StreamEvent struct {
	ID    string
	Type  string
	Delta string
}
