package engine

import "time"

// CommandComment is one review-trigger comment crq posted.
type CommandComment struct {
	ID        int64
	Bot       string
	CreatedAt time.Time
}

// TidyInput is everything the decision needs about one PR's trigger comments.
type TidyInput struct {
	// Commands are the trigger comments crq itself posted, oldest first.
	Commands []CommandComment
	// AnsweredAt is the newest moment each bot demonstrably acted — a review, a
	// completion reply, a classified event. A command with nothing after it was
	// never read, and the instruction is to remove only what has been.
	AnsweredAt map[string]time.Time
	// Live are the command IDs a round that has NOT progressed still depends on:
	// the open round's own command and its co-reviewer triggers.
	Live map[int64]bool
	// HeadAt is when the current head was committed. A command at or after it is
	// still adoptable, so removing it would make crq post a duplicate — unless
	// the round itself has already replaced it (Superseded).
	HeadAt time.Time
	// Superseded are commands the round explicitly moved past by posting a newer
	// one. They are exempt from the head check: crq's own record that it has
	// replaced a command is stronger evidence than any timestamp.
	Superseded map[int64]bool
}

// StaleCommands returns the trigger comments that can be deleted: crq asked, the
// bot answered, and the round that asked has moved on.
//
// Deleting a comment crq still reads is the way this becomes expensive. Three
// guards, and a command has to clear all of them:
//
//   - it belongs to no live round — the user's rule, and the one that matters:
//     only rounds that have already progressed;
//   - the bot acted after it, so it was actually read rather than merely old;
//   - it predates the current head, because adoption only ever considers
//     commands newer than the head commit. Delete one of those and the next
//     pump sees no command, posts another, and buys a second review.
//
// Anything crq did not post is not here at all: the caller only collects its own
// comments, so a human's "@coderabbitai review" is never a candidate.
func StaleCommands(in TidyInput) []int64 {
	var stale []int64
	for _, cmd := range in.Commands {
		if in.Live[cmd.ID] {
			continue
		}
		answered, ok := in.AnsweredAt[cmd.Bot]
		if !ok || answered.Before(cmd.CreatedAt) {
			continue
		}
		if !in.Superseded[cmd.ID] && !in.HeadAt.IsZero() && !cmd.CreatedAt.Before(in.HeadAt) {
			continue
		}
		stale = append(stale, cmd.ID)
	}
	return stale
}
