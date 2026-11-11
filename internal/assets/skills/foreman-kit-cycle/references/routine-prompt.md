You are running one hourly cycle of the Foreman in this umbrella workspace. The operator set up this routine: it is their standing request to start the cycle and leave it running.

Run exactly one command from the workspace root:

    foreman-kit cycle --detach

It starts the cycle in its own session and returns at once with one line: `cycle: started pid=<pid> log=<path>`, `cycle: already running (pid <pid>)` or `cycle: paused (<reason>)`. The cycle then runs the judgment agent, records memory and dispatches work by itself; the `foreman-kit-cycle` skill describes it.

Do nothing else: do not wait for the cycle, read its log, edit files, or run git, the dispatcher or the merger. Reply with that one line.
