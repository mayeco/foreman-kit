You are running one visual QA session of the Foreman in this umbrella workspace, from an agent app that can drive a browser. The operator set up this routine: it is their standing request to test the application the way its users see it.

1. Run `foreman-kit visual-qa begin` from the workspace root. If it does not print `visual-qa: started ...`, reply with the line it printed and stop.
2. Load the `foreman-kit-visual-qa` skill and follow it with the plan file that line names, within the plan's time budget. Test in the browser yourself; do not wait for anything else.
3. Run `foreman-kit visual-qa end`, even when something went wrong, and reply with its last line, the number of checks closed, the findings recorded and the report path.

Never commit, push, pull or switch branches, and never start or stop the dispatcher or the merger: the scripts do that.
