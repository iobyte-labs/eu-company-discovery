package fetch

import (
	"strconv"
	"strings"
	"time"
)

// robotsRules is one site's robots.txt, reduced to the group that applies to us.
//
// Only the group that applies is kept. Everything else in the file — other agents' rules,
// sitemaps, comments — is of no use to a crawler that has already decided which group it
// belongs to, and keeping it would invite someone to start interpreting it.
type robotsRules struct {
	// allowAll is true when the site published no robots.txt, or published one with no
	// rules for us.
	allowAll bool
	rules    []robotsRule
	// crawlDelay is the group's Crawl-delay, or 0 when it declared none. It is honoured in
	// addition to our own rate limit, and the slower of the two wins.
	crawlDelay time.Duration
	// agentGroup names the user-agent group the rules came from, for the message a blocked
	// page reports. "robots.txt disallows /members" is actionable; "blocked" is not.
	agentGroup string
}

type robotsRule struct {
	pattern string
	allow   bool
}

// allowAllRules is what an absent robots.txt means: a site that publishes no robots.txt has
// not restricted anything.
func allowAllRules() *robotsRules { return &robotsRules{allowAll: true} }

// parseRobots reads a robots.txt body and keeps the rules that apply to agentToken.
//
// Group selection follows RFC 9309: the most specific matching user-agent group wins, and
// "*" is the fallback. Lines that are not understood are ignored rather than being treated
// as a refusal, because a typo in someone's robots.txt must not stop the whole crawl — but
// a line that *is* understood is obeyed exactly.
//
// maxBytes bounds how much of the file is read; a robots.txt bigger than that has its tail
// ignored, which is what Google's parser does too.
func parseRobots(body []byte, agentToken string) *robotsRules {
	const maxBytes = 512 << 10
	if len(body) > maxBytes {
		body = body[:maxBytes]
	}
	agentToken = strings.ToLower(agentToken)

	type group struct {
		agents     []string
		rules      []robotsRule
		crawlDelay time.Duration
	}
	var groups []*group
	var current *group
	// Consecutive User-agent lines form one group; the first rule after them closes it, so
	// a later User-agent line starts a new group.
	startNewGroup := true

	for _, rawLine := range strings.Split(string(body), "\n") {
		line := rawLine
		if hash := strings.IndexByte(line, '#'); hash >= 0 {
			line = line[:hash]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)

		switch field {
		case "user-agent":
			if value == "" {
				continue
			}
			if startNewGroup || current == nil {
				current = &group{}
				groups = append(groups, current)
				startNewGroup = false
			}
			current.agents = append(current.agents, strings.ToLower(value))
		case "disallow", "allow":
			if current == nil {
				continue // a rule before any User-agent line belongs to nobody
			}
			startNewGroup = true
			// "Disallow:" with an empty value is the documented way to allow everything,
			// so it is dropped rather than recorded as a rule matching every path.
			if field == "disallow" && value == "" {
				continue
			}
			current.rules = append(current.rules, robotsRule{
				pattern: value, allow: field == "allow",
			})
		case "crawl-delay":
			if current == nil {
				continue
			}
			startNewGroup = true
			if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
				// A hostile or mistaken file could ask for a delay no run would survive.
				if seconds > 300 {
					seconds = 300
				}
				current.crawlDelay = time.Duration(seconds * float64(time.Second))
			}
		default:
			// Sitemap, Host, and anything else: not a crawler's instruction.
			startNewGroup = true
		}
	}

	// Pick the group that names us most specifically. A named agent scores its own length,
	// which is at least 1, so any group naming us beats the "*" group at 0.
	var best *group
	bestScore := -1
	for _, candidate := range groups {
		for _, agent := range candidate.agents {
			score := -1
			switch {
			case agent == "*":
				score = 0
			case agentToken != "" && strings.Contains(agentToken, agent):
				score = len(agent)
			}
			if score > bestScore {
				best, bestScore = candidate, score
			}
		}
	}
	if best == nil || len(best.rules) == 0 {
		rules := allowAllRules()
		if best != nil {
			rules.crawlDelay = best.crawlDelay
			rules.agentGroup = groupName(best.agents)
		}
		return rules
	}
	return &robotsRules{
		rules:      best.rules,
		crawlDelay: best.crawlDelay,
		agentGroup: groupName(best.agents),
	}
}

func groupName(agents []string) string {
	if len(agents) == 0 {
		return "*"
	}
	return agents[0]
}

// allows reports whether path may be fetched, and the rule that refused it when it may not.
//
// Longest match wins, and Allow beats Disallow on a tie — the rule from RFC 9309. It
// matters in practice: a site that disallows "/" and allows "/members" means the member
// list to be readable, and a parser that took the first match would refuse it.
func (r *robotsRules) allows(path string) (bool, string) {
	if r == nil || r.allowAll {
		return true, ""
	}
	if path == "" {
		path = "/"
	}
	bestLen, bestAllow, bestPattern := -1, true, ""
	for _, rule := range r.rules {
		if !matchRobotsPath(rule.pattern, path) {
			continue
		}
		length := len(rule.pattern)
		if length > bestLen || (length == bestLen && rule.allow) {
			bestLen, bestAllow, bestPattern = length, rule.allow, rule.pattern
		}
	}
	if bestLen < 0 || bestAllow {
		return true, ""
	}
	return false, bestPattern
}

// matchRobotsPath matches a robots.txt path pattern: "*" stands for any sequence and a
// trailing "$" anchors the end of the path.
//
// Written out rather than translated into a regular expression on purpose. The pattern
// comes from somebody else's server, and handing an untrusted string to a regexp engine to
// match every URL is a cost nobody needs to pay here.
func matchRobotsPath(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	rest := path[len(parts[0]):]
	for index := 1; index < len(parts); index++ {
		part := parts[index]
		if part == "" {
			// A trailing "*" matches whatever is left, including nothing.
			continue
		}
		if index == len(parts)-1 && anchored {
			// The last literal of an anchored pattern has to end the path, so look for it
			// at the end rather than at its first occurrence: "/*.pdf$" must match
			// "/a.pdf.pdf".
			return strings.HasSuffix(rest, part)
		}
		at := strings.Index(rest, part)
		if at < 0 {
			return false
		}
		rest = rest[at+len(part):]
	}
	if anchored && !strings.HasSuffix(pattern, "*") {
		return rest == ""
	}
	return true
}
