package web

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// noTarget names builds of a Dockerfile's last stage (no --opt target)
// when nothing they pushed names them.
const noTarget = "last stage (no target)"

// buildName is what one build built: its target, or, without one, the
// repository it pushed to, or the Dockerfile's last stage.
func buildName(target string, images []string) string {
	if target != "" {
		return target
	}
	if rs := history.Repositories(images); len(rs) > 0 {
		if len(rs) > 1 {
			return rs[0] + " +" + strconv.Itoa(len(rs)-1)
		}
		return rs[0]
	}
	return noTarget
}

// imageName names an image of the lists (a target): a target by its
// name; builds without one by the repositories they pushed to, the most
// recent first and the others counted (imageTitle lists them all), or
// the Dockerfile's last stage when they pushed nothing.
func imageName(target string, repos []string) string {
	if target != "" {
		return target
	}
	switch len(repos) {
	case 0:
		return noTarget
	case 1:
		return repos[0]
	}
	return repos[0] + " +" + strconv.Itoa(len(repos)-1)
}

// imageTitle lists every repository behind an imageName that counts the
// others; "" when the name says it all.
func imageTitle(target string, repos []string) string {
	if target != "" || len(repos) < 2 {
		return ""
	}
	return strings.Join(repos, ", ")
}

// sameImage says whether two builds are of one image: the same target,
// and without one the same repository pushed first (as the store pairs a
// build with its previous one).
func sameImage(targetA string, imagesA []string, targetB string, imagesB []string) bool {
	if targetA != targetB {
		return false
	}
	return targetA != "" || firstRepository(imagesA) == firstRepository(imagesB)
}

func firstRepository(images []string) string {
	if rs := history.Repositories(images); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// imageNames is imageName for a project's targets; without the names (an
// older controller) the targets name themselves.
func (s *Server) imageNames(ctx context.Context, p store.ProjectKey, since time.Time) map[string][]string {
	m, err := s.data.TargetImages(ctx, p, since)
	if err != nil {
		return nil
	}
	return m
}

// targetOf is the target an image key of an address stands for: "-" is
// "no target" (a stage name cannot be "-").
func targetOf(key string) (target string, none bool) {
	if key == "-" {
		return "", true
	}
	return key, false
}
