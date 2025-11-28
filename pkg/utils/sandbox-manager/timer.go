package utils

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var TimerParser = regexp.MustCompile(`This timer will be triggered after (\d+) seconds`)

var TimerPrefix = "SandboxTimer."

func CheckAndLoadTimerFromCondition(conditionType string, message string, lastTransitionTime time.Time,
	callback func(after time.Duration, eventType consts.EventType)) error {
	if !strings.HasPrefix(conditionType, TimerPrefix) {
		return nil
	}
	var err error
	eventName := strings.TrimPrefix(conditionType, TimerPrefix)
	eventType := consts.EventType(eventName)
	matches := TimerParser.FindStringSubmatch(message)
	if len(matches) != 2 {
		return fmt.Errorf("failed to parse time from message")
	}
	seconds, err := strconv.Atoi(matches[1])
	if err != nil { // 正则规则保证了理论上不可能进入这个分支
		return fmt.Errorf("failed to parse seconds from message: %v", err)
	}
	after := time.Until(lastTransitionTime.Add(time.Duration(seconds) * time.Second))
	callback(after, eventType)
	return nil
}

func GenerateTimerCondition(afterSeconds int, event consts.EventType, triggered bool, result string) (key, status, reason, message string) {
	conditionKey := "SandboxTimer." + string(event)
	if triggered {
		return conditionKey, string(metav1.ConditionTrue), "Triggered", result
	} else {
		return conditionKey, string(metav1.ConditionFalse), "SaveTimer",
			fmt.Sprintf("This timer will be triggered after %d seconds", afterSeconds)
	}
}
