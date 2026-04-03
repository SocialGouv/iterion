// Package userinput provides types for the item/tool/requestUserInput protocol.
package userinput

import "context"

// QuestionOption represents a selectable choice within a question.
type QuestionOption struct {
	Label       string
	Description string
}

// Question represents a single question posed to the user.
type Question struct {
	ID       string
	Header   string
	Question string
	IsOther  bool
	IsSecret bool
	Options  []QuestionOption // nil means free text input
}

// Answer contains the user's response(s) to a question.
type Answer struct {
	Answers []string
}

// Request represents the full user input request from the CLI.
type Request struct {
	ItemID    string
	ThreadID  string
	TurnID    string
	Questions []Question
}

// Response contains the answers to all questions keyed by question ID.
type Response struct {
	Answers map[string]*Answer
}

// Callback is invoked when the CLI sends an item/tool/requestUserInput request.
type Callback func(ctx context.Context, req *Request) (*Response, error)
