package config

import (
	"strings"
	"testing"
)

type nginxBlock struct {
	header     string
	start, end int
	parent     int
}

// parseNginxBlocks builds the small portion of nginx's block structure needed
// by these tests. nginx.conf keeps block delimiters on their own lines (or at
// the end of a block header), so quoted response bodies containing braces do
// not look like syntax here.
func parseNginxBlocks(t *testing.T, conf string) ([]string, []nginxBlock) {
	t.Helper()
	lines := strings.Split(conf, "\n")
	blocks := make([]nginxBlock, 0)
	stack := make([]int, 0)

	for lineNumber, raw := range lines {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		if line == "}" {
			if len(stack) == 0 {
				t.Fatalf("nginx.conf has an unmatched closing brace on line %d", lineNumber+1)
			}
			blocks[stack[len(stack)-1]].end = lineNumber
			stack = stack[:len(stack)-1]
			continue
		}
		if !strings.HasSuffix(line, "{") {
			continue
		}

		parent := -1
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		blocks = append(blocks, nginxBlock{
			header: strings.TrimSpace(strings.TrimSuffix(line, "{")),
			start:  lineNumber,
			end:    -1,
			parent: parent,
		})
		stack = append(stack, len(blocks)-1)
	}
	if len(stack) != 0 {
		t.Fatalf("nginx.conf has %d unclosed block(s)", len(stack))
	}
	return lines, blocks
}

func childBlocks(blocks []nginxBlock, parent int) []int {
	var children []int
	for i := range blocks {
		if blocks[i].parent == parent {
			children = append(children, i)
		}
	}
	return children
}

// directDirectives returns directives declared directly in a block, excluding
// nested blocks. That distinction matters because proxy_set_header directives
// are inherited only when the current block declares none of its own.
func directDirectives(lines []string, blocks []nginxBlock, block int, name string) [][]string {
	children := childBlocks(blocks, block)
	var directives [][]string
	for lineNumber := blocks[block].start + 1; lineNumber < blocks[block].end; lineNumber++ {
		skipped := false
		for _, child := range children {
			if lineNumber >= blocks[child].start && lineNumber <= blocks[child].end {
				lineNumber = blocks[child].end
				skipped = true
				break
			}
		}
		if skipped {
			continue
		}
		line := strings.TrimSpace(strings.SplitN(lines[lineNumber], "#", 2)[0])
		fields := strings.Fields(strings.TrimSuffix(line, ";"))
		if len(fields) > 0 && fields[0] == name {
			directives = append(directives, fields)
		}
	}
	return directives
}

// selectedLocation models the exact-match-then-longest-prefix rule nginx uses
// for the location forms present in this config.
func selectedLocation(t *testing.T, blocks []nginxBlock, parent int, path string) int {
	t.Helper()
	best, bestLength := -1, -1
	for _, child := range childBlocks(blocks, parent) {
		fields := strings.Fields(blocks[child].header)
		if len(fields) < 2 || fields[0] != "location" || strings.HasPrefix(fields[1], "@") {
			continue
		}
		if fields[1] == "=" {
			if len(fields) != 3 {
				t.Fatalf("cannot parse exact location header %q", blocks[child].header)
			}
			if fields[2] == path {
				return child
			}
			continue
		}
		if len(fields) != 2 || strings.HasPrefix(fields[1], "~") || fields[1] == "^~" {
			t.Fatalf("test location selector does not support header %q", blocks[child].header)
		}
		if strings.HasPrefix(path, fields[1]) && len(fields[1]) > bestLength {
			best, bestLength = child, len(fields[1])
		}
	}
	if best == -1 {
		return -1
	}
	if nested := selectedLocation(t, blocks, best, path); nested != -1 {
		return nested
	}
	return best
}

func TestGatewayContributorWebSocketUpgrade(t *testing.T) {
	lines, blocks := parseNginxBlocks(t, readNginxConf(t))

	server := -1
	for i := range blocks {
		if blocks[i].header == "server" {
			server = i
			break
		}
	}
	if server == -1 {
		t.Fatal("nginx.conf has no server block")
	}

	selected := selectedLocation(t, blocks, server, "/api/contribute/ws")
	if selected == -1 {
		t.Fatal("nginx.conf has no location matching /api/contribute/ws")
	}

	// Walk from the server to the selected location, replacing rather than
	// merging each declared proxy_set_header set to match nginx inheritance.
	var chain []int
	for block := selected; block != -1; block = blocks[block].parent {
		chain = append(chain, block)
		if block == server {
			break
		}
	}
	headers := make(map[string]string)
	for i := len(chain) - 1; i >= 0; i-- {
		directives := directDirectives(lines, blocks, chain[i], "proxy_set_header")
		if len(directives) == 0 {
			continue
		}
		headers = make(map[string]string)
		for _, directive := range directives {
			if len(directive) == 3 {
				headers[strings.ToLower(directive[1])] = directive[2]
			}
		}
	}

	for name, want := range map[string]string{
		"upgrade":    "$http_upgrade",
		"connection": "$connection_upgrade",
	} {
		if got := headers[name]; got != want {
			t.Errorf("%s selects %q with effective %s header %q, want %q", "/api/contribute/ws", blocks[selected].header, name, got, want)
		}
	}

	mapFound := false
	for i := range blocks {
		if blocks[i].header != "map $http_upgrade $connection_upgrade" {
			continue
		}
		mapFound = true
		defaults := directDirectives(lines, blocks, i, "default")
		if len(defaults) != 1 || len(defaults[0]) != 2 || defaults[0][1] != "upgrade" {
			t.Error("map $http_upgrade $connection_upgrade must default to upgrade")
		}
	}
	if !mapFound {
		t.Fatal("nginx.conf is missing map $http_upgrade $connection_upgrade")
	}
}
