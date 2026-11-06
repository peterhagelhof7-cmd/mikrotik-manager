package main

import "context"

func (p *Poller) setDeviceUser(ctx context.Context, id, user, pw string) []Result {
	return p.SetUserPassword(ctx, []string{id}, user, pw)
}

func (p *Poller) deviceCredUser(id string) string {
	c, _ := p.vault.For(id)
	return c.User
}
