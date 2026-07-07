package nftables

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	log "log/slog"
	"net"
	"os"
	"reflect"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/userdata"
)

const (
	TableFilter = "filter"

	ipv4SrcOffset = 12
	ipv4DstOffset = 16
	ipv6SrcOffset = 8
	ipv6DstOffset = 24
)

type Client struct {
	conn   *nftables.Conn
	family nftables.TableFamily
}

func NewClient(family nftables.TableFamily) (*Client, error) {
	conn, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create nftables client connection: %w", err)
	}
	return &Client{
		conn:   conn,
		family: family,
	}, nil
}

func (c *Client) Close() error {
	if err := c.conn.CloseLasting(); err != nil {
		return fmt.Errorf("failed to close nftables client: %w", err)
	}
	return nil
}

func (c *Client) Flush() {
	c.conn.Flush()
}

func (c *Client) GetTable(name string) *nftables.Table {
	return &nftables.Table{
		Family: c.family,
		Name:   name,
	}
}

func (c *Client) GetChain(table, chain string) (*nftables.Chain, error) {
	ch, err := c.conn.ListChain(c.GetTable(table), chain)
	if err != nil {
		return nil, err
	}
	return ch, nil
}

func (c *Client) CheckChain(table, chain string) bool {
	_, err := c.GetChain(table, chain)
	return err == nil
}

func (c *Client) AddChain(chain *nftables.Chain) *nftables.Chain {
	ch, _ := c.GetChain(chain.Table.Name, chain.Name)

	if ch == nil {
		ch = c.conn.AddChain(chain)
		c.Flush()
	}
	return ch
}

func (c *Client) DeleteChain(chain *nftables.Chain) {
	c.conn.DelChain(chain)
}

func (c *Client) InsertUnique(rule *nftables.Rule) (*nftables.Rule, error) {
	comment, _ := userdata.GetString(rule.UserData, userdata.TypeComment)
	log.Debug("inserting rule", "table", rule.Table.Name, "chain", rule.Chain.Name, "comment", comment)
	return c.add(rule, c.conn.InsertRule)
}

func (c *Client) AddUnique(rule *nftables.Rule) (*nftables.Rule, error) {
	comment, _ := userdata.GetString(rule.UserData, userdata.TypeComment)
	log.Debug("adding rule", "table", rule.Table.Name, "chain", rule.Chain.Name, "comment", comment)
	return c.add(rule, c.conn.AddRule)
}

func (c *Client) add(rule *nftables.Rule, f func(r *nftables.Rule) *nftables.Rule) (*nftables.Rule, error) {
	r, err := c.Exists(rule)
	if err != nil {
		return r, err
	}

	if r == nil {
		r = f(rule)
	}

	return r, nil
}

func (c *Client) FindRuleByComment(table *nftables.Table, chain *nftables.Chain, comment string) (*nftables.Rule, error) {
	rules, err := c.conn.GetRules(table, chain)
	if err != nil {
		return nil, fmt.Errorf("failed to list rules: %w", err)
	}

	ud := UserDataComment(comment)

	for _, r := range rules {
		if bytes.Equal(r.UserData, ud) {
			return r, nil
		}
	}

	return nil, nil
}

func (c *Client) DeleteRule(r *nftables.Rule) error {
	if err := c.conn.DelRule(r); err != nil {
		return fmt.Errorf("failed to delete rule: %w", err)
	}
	return nil
}

func (c *Client) List(t *nftables.Table, ch *nftables.Chain) ([]*nftables.Rule, error) {
	rules, err := c.conn.GetRules(t, ch)
	if err != nil {
		return nil, fmt.Errorf("failed to list rules in table %q, chain %q: %w", t.Name, ch.Name, err)
	}
	return rules, nil
}

func (c *Client) Exists(rule *nftables.Rule) (*nftables.Rule, error) {
	rules, err := c.conn.GetRules(rule.Table, rule.Chain)
	if err != nil {
		return nil, fmt.Errorf("failed to list rules in table %q, chain %q: %w", rule.Table.Name, rule.Chain.Name, err)
	}

	var existing *nftables.Rule
	cnt := 0
	for _, r := range rules {
		if ruleEqual(rule, r) {
			existing = r
			cnt++
		}
	}

	if cnt == 0 {
		return nil, nil
	}

	if cnt > 1 {
		comment, ok := userdata.GetString(rule.UserData, userdata.TypeComment)
		if !ok {
			log.Warn("failed to get comment for rule", "handle", rule.Handle, "table", rule.Table.Name, "chain", rule.Chain.Name)
		} else {
			log.Warn("too many rules", "present", cnt, "rule", comment, "table", rule.Table.Name, "chain", rule.Chain.Name)
		}
	}

	return existing, nil
}

func (c *Client) GetLen() uint32 {
	if c.family == nftables.TableFamilyIPv6 {
		return net.IPv6len
	}
	return net.IPv4len
}

func (c *Client) GetDstOffset() uint32 {
	if c.family == nftables.TableFamilyIPv6 {
		return ipv6DstOffset
	}
	return ipv4DstOffset
}

func (c *Client) GetSrcOffset() uint32 {
	if c.family == nftables.TableFamilyIPv6 {
		return ipv6SrcOffset
	}
	return ipv4SrcOffset
}

func (c *Client) UpdateSet(set *nftables.Set, elements []nftables.SetElement) error {
	existingSet, err := c.conn.GetSetByName(set.Table, set.Name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to get set: %w", err)
	}

	exists := err == nil && existingSet != nil

	if exists {
		existingElements, err := c.conn.GetSetElements(existingSet)
		if err != nil {
			return fmt.Errorf("failed to get elements for set %q, table %q", existingSet.Name, existingSet.Table.Name)
		}

		toAdd, toDel := processElements(elements, existingElements)

		if len(toAdd) > 0 || len(toDel) > 0 {
			log.Debug("updating set", "name", existingSet.Name, "table", existingSet.Table.Name)
			if len(toDel) > 0 {
				if err := c.conn.SetDeleteElements(existingSet, toDel); err != nil {
					return fmt.Errorf("failed to remove elements from set %q: %w", existingSet.Name, err)
				}
			}

			if len(toAdd) > 0 {
				if err := c.conn.SetAddElements(existingSet, toAdd); err != nil {
					return fmt.Errorf("failed to add elements to set %q: %w", existingSet.Name, err)
				}
			}
		}

		return nil
	}

	log.Debug("adding set", "name", set.Name, "table", set.Table)
	if err := c.conn.AddSet(set, elements); err != nil {
		return fmt.Errorf("failed to add set %q to table %q: %w", set.Name, set.Table.Name, err)
	}

	return nil
}

func (c *Client) DeleteSet(table *nftables.Table, name string) error {
	setName, err := hash(name)
	if err != nil {
		return fmt.Errorf("failed to hash name: %w", err)
	}

	set, err := c.conn.GetSetByName(table, setName)
	if err != nil {
		return fmt.Errorf("failed to get set: %w", err)
	}
	if set != nil {
		c.conn.DelSet(set)
	}
	return nil
}

func (c *Client) NewSet(chain *nftables.Chain, name string) (*nftables.Set, error) {
	setName, err := hash(name)
	if err != nil {
		return nil, fmt.Errorf("failed to hash name: %w", err)
	}

	return &nftables.Set{
		Table:   chain.Table,
		Name:    setName,
		Comment: name,
		KeyType: nftables.TypeInetService,
	}, nil
}

func hash(data string) (string, error) {
	h := fnv.New32a()
	if _, err := h.Write([]byte(data)); err != nil {
		return "", fmt.Errorf("failed to generate hash: %w", err)
	}

	return fmt.Sprintf("%x", h.Sum32()), nil
}

func processElements(newEls, existingEls []nftables.SetElement) (toAdd, toDel []nftables.SetElement) {
	toAdd = findNonCommon(newEls, existingEls)
	toDel = findNonCommon(existingEls, newEls)
	return
}

func findNonCommon(a, b []nftables.SetElement) []nftables.SetElement {
	nonCommon := []nftables.SetElement{}
	for i := range a {
		if !isPresent(a[i], b) {
			nonCommon = append(nonCommon, a[i])
		}
	}
	return nonCommon
}

func isPresent(toCheck nftables.SetElement, elements []nftables.SetElement) bool {
	for _, e := range elements {
		if slices.Compare(toCheck.Key, e.Key) == 0 {
			return true
		}
	}

	return false
}

func ruleEqual(a, b *nftables.Rule) bool {
	if a.Chain.Name != b.Chain.Name {
		return false
	}
	if a.Table.Name != b.Table.Name {
		return false
	}

	if !bytes.Equal(a.UserData, b.UserData) {
		return false
	}

	for i := range a.Exprs {
		switch a.Exprs[i].(type) {
		case *expr.Meta:
			if !exprEqual(&expr.Meta{}, a.Exprs[i], b.Exprs[i]) {
				return false
			}
		case *expr.Lookup:
			if !exprEqual(&expr.Lookup{}, a.Exprs[i], b.Exprs[i]) {
				return false
			}
		case *expr.Verdict:
			if !exprEqual(&expr.Verdict{}, a.Exprs[i], b.Exprs[i]) {
				return false
			}
		case *expr.Cmp:
			if !exprEqual(&expr.Cmp{}, a.Exprs[i], b.Exprs[i]) {
				return false
			}
		case *expr.Payload:
			if !exprEqual(&expr.Payload{}, a.Exprs[i], b.Exprs[i]) {
				return false
			}
		case *expr.Ct:
			if !exprEqual(&expr.Ct{}, a.Exprs[i], b.Exprs[i]) {
				return false
			}
		case *expr.Bitwise:
			if !exprEqual(&expr.Bitwise{}, a.Exprs[i], b.Exprs[i]) {
				return false
			}
		}
	}

	return true
}

func exprEqual[V *expr.Meta | *expr.Lookup | *expr.Verdict | *expr.Cmp | *expr.Payload | *expr.Ct | *expr.Bitwise](_ V, aExpr, bExpr expr.Any) bool {
	aExprCast, ok := aExpr.(V)
	if !ok {
		return false
	}
	bExprCast, ok := bExpr.(V)
	if !ok {
		return false
	}
	if reflect.DeepEqual(aExprCast, bExprCast) {
		return true
	}
	return false
}

func UserDataComment(comment string) []byte {
	return userdata.AppendString([]byte{}, userdata.TypeComment, comment)
}
