package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// actionAdvisoryCollector is opt-in, per composite command. Normal CLI calls
// keep their existing stderr warnings; manufacturing release additionally
// records every nested action advisory so it can fail closed at phase gates.
type actionAdvisoryCollector struct {
	mu     sync.Mutex
	issues []string
}

func (c *actionAdvisoryCollector) observe(action string, body []byte) {
	if c == nil {
		return
	}
	var env struct {
		Warnings         []string `json:"warnings"`
		StaleRisk        string   `json:"staleRisk"`
		ConcurrentWriter string   `json:"concurrentWriter"`
	}
	if json.Unmarshal(body, &env) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, warning := range env.Warnings {
		if warning = strings.TrimSpace(warning); warning != "" {
			c.issues = append(c.issues, fmt.Sprintf("%s warning: %s", action, warning))
		}
	}
	if env.StaleRisk != "" {
		c.issues = append(c.issues, fmt.Sprintf("%s staleRisk: %s", action, env.StaleRisk))
	}
	if env.ConcurrentWriter != "" {
		c.issues = append(c.issues, fmt.Sprintf("%s concurrentWriter: %s", action, env.ConcurrentWriter))
	}
}

func (c *actionAdvisoryCollector) error() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.issues) == 0 {
		return nil
	}
	return fmt.Errorf("runtime reliability evidence is not clean: %s", strings.Join(c.issues, "; "))
}
