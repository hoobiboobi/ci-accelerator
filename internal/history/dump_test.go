package history

import (
	"fmt"
	"os"
	"testing"
)

func TestDump(t *testing.T) {
	if os.Getenv("DUMP") == "" {
		t.Skip()
	}
	bs := loadFixtures(t)
	for _, b := range bs {
		a := b.Attribution
		fmt.Printf("%s %s p%d j%d %s target=%s args=%v err=%q ctx=%d\n", b.Ref, a.State, a.Pipeline, a.Job, a.TraceID, b.Target, b.BuildArgs, b.Error, b.ContextBytes)
		for _, s := range b.Steps {
			fmt.Printf("    %-11s %-8s line=%d id=%s %6v from=%s err=%q %s\n", s.Key, s.State, s.Line, s.Identity, s.Duration().Round(1e6), s.ReceivedFrom, s.Error, s.Name)
		}
	}
}
