package network

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/go-iptables/iptables"
	log "github.com/sirupsen/logrus"
)

// most of it is proudly found elsewhere https://github.com/flannel-io/flannel/blob/master/network/iptables.go

type IPTables interface {
	AppendUnique(table string, chain string, rulespec ...string) error
	Delete(table string, chain string, rulespec ...string) error
	Exists(table string, chain string, rulespec ...string) (bool, error)
}

type IPTablesError interface {
	IsNotExist() bool
	Error() string
}

type IPTablesRule struct {
	Table    string
	Chain    string
	Rulespec []string
}

func NewIPTables() (*iptables.IPTables, error) {
	return iptables.New()
}

func ipTablesRulesExist(ipt IPTables, rules []IPTablesRule) (bool, error) {
	for _, rule := range rules {
		exists, err := ipt.Exists(rule.Table, rule.Chain, rule.Rulespec...)
		if err != nil {
			// this shouldn't ever happen
			return false, fmt.Errorf("failed to check rule existence: %v", err)
		}
		if !exists {
			return false, nil
		}
	}

	return true, nil
}

func SetupAndEnsureIPTables(ctx context.Context, rules []IPTablesRule, resyncPeriod int) {
	ipt, err := iptables.New()
	if err != nil {
		// if we can't find iptables, give up and return
		log.Errorf("Failed to setup IPTables. iptables binary was not found: %v", err)
		return
	}

	ensureCtx, ensureCancel := context.WithCancel(ctx)
	defer ensureCancel()

	for {
		select {
		case <-ensureCtx.Done():
			if err := teardownIPTables(ipt, rules); err != nil {
				log.Errorf("Failed to teardown iptables rules: %v", err)
			}
			return
		default:
			// Ensure that all the iptables rules exist every resync period
			if err := ensureIPTables(ipt, rules); err != nil {
				log.Errorf("Failed to ensure iptables rules: %v", err)
			}
		}
		time.Sleep(time.Duration(resyncPeriod) * time.Second)
	}
}

// DeleteIPTables delete specified iptables rules
func DeleteIPTables(rules []IPTablesRule) error {
	ipt, err := iptables.New()
	if err != nil {
		// if we can't find iptables, give up and return
		log.Errorf("Failed to setup IPTables. iptables binary was not found: %v", err)
		return err
	}
	if err := teardownIPTables(ipt, rules); err != nil {
		log.Errorf("Failed to teardown iptables rules: %v", err)
	}
	return nil
}

func ensureIPTables(ipt IPTables, rules []IPTablesRule) error {
	exists, err := ipTablesRulesExist(ipt, rules)
	if err != nil {
		return fmt.Errorf("Error checking rule existence: %v", err)
	}
	if exists {
		// if all the rules already exist, no need to do anything
		return nil
	}
	// Otherwise, teardown all the rules and set them up again
	// We do this because the order of the rules is important
	log.Info("Some iptables rules are missing; deleting and recreating rules")
	if err = teardownIPTables(ipt, rules); err != nil {
		return fmt.Errorf("Error tearing down rules: %v", err)
	}
	if err = setupIPTables(ipt, rules); err != nil {
		return fmt.Errorf("Error setting up rules: %v", err)
	}
	return nil
}

func setupIPTables(ipt IPTables, rules []IPTablesRule) error {
	for _, rule := range rules {
		log.Info("Adding iptables rule: ", strings.Join(rule.Rulespec, " "))
		err := ipt.AppendUnique(rule.Table, rule.Chain, rule.Rulespec...)
		if err != nil {
			return fmt.Errorf("failed to insert IPTables rule: %v", err)
		}
	}
	return nil
}

func teardownIPTables(ipt IPTables, rules []IPTablesRule) error {
	for _, rule := range rules {
		log.Info("Deleting iptables rule: ", strings.Join(rule.Rulespec, " "))
		err := ipt.Delete(rule.Table, rule.Chain, rule.Rulespec...)
		if err != nil {
			e := err.(IPTablesError)
			// If this error is because the rule is already deleted, the message from iptables will be
			// "Bad rule (does a matching rule exist in that chain?)". These are safe to ignore.
			// However other errors (like EAGAIN caused by other things not respecting the xtables.lock)
			// should halt the ensure process.  Otherwise rules can get out of order when a rule we think
			// is deleted is actually still in the chain.
			// This will leave the rules incomplete until the next successful reconciliation loop.
			if !e.IsNotExist() {
				return err
			}
		}
	}
	return nil
}
