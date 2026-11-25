#include <os/log.h>

#include "bridge.h"

// Instances started by a LaunchAgent have no terminal, so diagnostics go to the system unified log; one entry per line,
// with the text redacted on the Go side.
// The log object is created lazily on the first call; callers guarantee that calls are serialized.
void fr_os_log_line(const char *line)
{
    static os_log_t agent_log;
    if (agent_log == NULL) {
        agent_log = os_log_create("io.findrelay.agent", "run");
    }
    os_log(agent_log, "%{public}s", line);
}
