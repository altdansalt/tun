package client

import "context"

const runnersUsage = `usage: tun runners

Print the server's live runners (machines running its jobs via tun runner), one JSON object
per line: {name, slots, running, last_seen, sandbox, network_isolation, source}. Nothing if it has none; its
own job slots are local_slots in $TUN_URL/v1/healthz.
`

func runnersCmd(args []string, e Env) int {
	fs := newFlags("runners", runnersUsage, e)
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	if len(pos) > 0 {
		return errorf(e, "usage: tun runners")
	}
	h, err := newClient(e).Health(context.Background())
	if err != nil {
		return errorf(e, "%v", err)
	}
	for _, r := range h.Runners {
		printJSON(e, r)
	}
	return 0
}
