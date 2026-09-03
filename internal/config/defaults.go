package config

import (
	"strings"
	"time"
)

func (c *Config) applyDefaults() {
	if c.Service.Metrics.Interval == 0 {
		c.Service.Metrics.Interval = Duration(5 * time.Second)
	}
	if m := c.Service.Managed; m != nil {
		if m.StopSignal == "" {
			m.StopSignal = "TERM"
		}
		if m.Readiness.Timeout == 0 {
			m.Readiness.Timeout = Duration(5 * time.Minute)
		}
		if m.Readiness.Interval == 0 {
			m.Readiness.Interval = Duration(1 * time.Second)
		}
		if m.StopTimeout == 0 {
			m.StopTimeout = Duration(30 * time.Second)
		}
	}
	if c.K6.StateEnv == "" {
		c.K6.StateEnv = "STATE_FILE"
	}
	if c.Report.OutputDir == "" {
		c.Report.OutputDir = "./reports"
	}
	if len(c.Report.Formats) == 0 {
		c.Report.Formats = []string{"markdown", "json"}
	}
	if c.Init.CommandTimeout == 0 {
		c.Init.CommandTimeout = Duration(5 * time.Minute)
	}
	applyStepDefaults(c.Init.Steps)
	applyStepDefaults(c.Teardown.Steps)

	for i := range c.K6.Steps {
		step := &c.K6.Steps[i]
		if step.Method == "" {
			step.Method = "GET"
		}
		step.Method = strings.ToUpper(step.Method)
	}

	for i := range c.K6.Setup {
		step := &c.K6.Setup[i]
		if step.Method == "" {
			step.Method = "GET"
		}
		step.Method = strings.ToUpper(step.Method)
	}
}

func applyStepDefaults(steps []Step) {
	for i := range steps {
		step := &steps[i]
		if step.Method == "" {
			step.Method = "GET"
		}
		step.Method = strings.ToUpper(step.Method)
		if step.Count == "" {
			step.Count = "1"
		}
		applyStepDefaults(step.Children)
	}
}
