#ifndef FINDRELAY_DARWIN_BRIDGE_H
#define FINDRELAY_DARWIN_BRIDGE_H

#include <ApplicationServices/ApplicationServices.h>
#include <CoreFoundation/CoreFoundation.h>
#include <limits.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

// Maximum number of roles walked up the AX parent chain, and the storage length of a single role string.
#define FR_AX_MAX_DEPTH 6
#define FR_AX_ROLE_STORE 40

typedef struct {
    double x;
    double y;
    double width;
    double height;
} fr_rect_t;

typedef enum {
    FR_PROC_NONE = 0,
    FR_PROC_DOCK = 1,
    FR_PROC_FINDER = 2,
    FR_PROC_OTHER = 3,
} fr_process_kind_t;

typedef enum {
    FR_URL_NONE = 0,
    FR_URL_FINDER_APP = 1,
    FR_URL_OTHER_APP = 2,
    FR_URL_FILE = 3,
    FR_URL_NON_FILE = 4,
} fr_url_kind_t;

typedef enum {
    FR_TAP_PASS = 0,
    FR_TAP_SWALLOW = 1,
    FR_TAP_CANCEL_UP = 2,
    FR_TAP_CANCEL_DESKTOP_UP = 3,
} fr_tap_disposition_t;

typedef enum {
    FR_DIR_ELIGIBLE = 0,
    FR_DIR_ERR_PERMISSION = 1,
    FR_DIR_ERR_IO = 2,
    FR_DIR_REJECT_NOT_FOUND = 3,
    FR_DIR_REJECT_SYMLINK = 4,
    FR_DIR_REJECT_NOT_DIRECTORY = 5,
    FR_DIR_REJECT_PACKAGE = 6,
    FR_DIR_REJECT_ALIAS = 7,
    FR_DIR_REJECT_VOLUME = 8,
    FR_DIR_REJECT_NON_LOCAL_VOLUME = 9,
    FR_DIR_REJECT_EXTERNAL_VOLUME = 10,
    FR_DIR_REJECT_UBIQUITOUS = 11,
    FR_DIR_REJECT_AMBIGUOUS_METADATA = 12,
} fr_dir_status_t;

typedef struct {
    int ax_error;
    bool budget_exceeded;
    int32_t pid;
    uint64_t proc_gen;
    fr_process_kind_t proc_kind;
    bool is_dock_finder;
    uint64_t element_hash;
    bool has_selection_info;
    int32_t selection_count;
    bool is_single_selected_self;
    char role[64];
    char subrole[64];
    char parent_chain[256];
    // Role sequence from the hit element up to the application (index 0 is the hit element), used by the Go side to
    // classify the container by structure.
    char roles[FR_AX_MAX_DEPTH][FR_AX_ROLE_STORE];
    int32_t role_count;
    fr_url_kind_t url_kind;
    char file_path[PATH_MAX];
    // New reference to the second level of the active-mode Finder parent chain when it is an AXGroup; the desktop
    // icon's selection info is read from it. The caller releases it.
    AXUIElementRef parent_group;
} fr_ax_hit_t;

typedef struct fr_tap_s fr_tap_t;

// Result code of one startup warm-up step; negative values are AX error codes.
typedef enum {
    FR_WARM_OK = 0,             // Success
    FR_WARM_NO_PROCESS = 1,     // Dock or Finder process not found
    FR_WARM_ICON_NOT_FOUND = 2, // No Finder icon in Dock's AX tree
    FR_WARM_BUDGET = 3,         // Total budget used up; the step did not complete
    FR_WARM_UNEXPECTED = 4,     // The hit result is not the expected Dock Finder icon
    FR_WARM_NOT_VISIBLE = 5,    // System-wide hit landed elsewhere (Dock auto-hidden or full-screen Space); query completed
} fr_warm_status_t;

// Monotonic clock (CLOCK_UPTIME_RAW, nanoseconds).
uint64_t fr_monotonic_now_ns(void);

// Maximum system double-click interval (NSEvent.doubleClickInterval, nanoseconds).
uint64_t fr_double_click_interval_ns(void);

// Security session query (Security.framework SessionGetInfo).
int fr_current_session_info(uint32_t *out_session_id, bool *out_has_graphic_access);

// Accessibility permission preflight.
bool fr_ax_is_trusted(bool prompt);

// Writes one line of redacted diagnostics to the system unified log (subsystem io.findrelay.agent).
void fr_os_log_line(const char *line);

// Helper that projects points inward from outer display edges (probes 8pt inward only within 1pt of an outer edge;
// shared edges are unchanged).
void fr_project_dock_edge_point(
    double x,
    double y,
    const fr_rect_t *displays,
    uint32_t display_count,
    double *out_x,
    double *out_y
);

// System-wide AX object management and bounded hit testing.
AXUIElementRef fr_ax_create_system_wide(void);
void fr_ax_release(AXUIElementRef ref);

// Performs one bounded hit test on scope: when scope is the system-wide element it takes the topmost element by window
// layering; when it is an application element it hits only within that app.
// A CannotComplete that returns quickly is treated as a cold start and retried once; no value at an outer display edge
// is projected inward and retried once.
// On success returns a new reference (released by the caller); otherwise returns NULL and reports the AX error code.
AXUIElementRef fr_ax_hit(
    AXUIElementRef system_ax,
    AXUIElementRef scope,
    double x,
    double y,
    uint64_t deadline_ns,
    int *out_ax_err,
    bool *budget_exceeded
);
bool fr_ax_elements_equal(AXUIElementRef a, AXUIElementRef b);

// Inspects an already-hit element (the caller owns el): process identity, plus the attributes along the Dock or Finder
// parent chain.
void fr_ax_inspect(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    uint64_t deadline_ns,
    const char *finder_app_path,
    bool inspect_desktop,
    bool collect_observe_chain,
    fr_ax_hit_t *out_hit
);
// Reads the URL and selection info after confirming the desktop icon container; when rechecking the second desktop
// click, only the selection info is refreshed.
void fr_ax_read_desktop_evidence(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    AXUIElementRef group,
    uint64_t deadline_ns,
    const char *finder_app_path,
    fr_ax_hit_t *out_hit
);
void fr_ax_read_selection(
    AXUIElementRef system_ax,
    AXUIElementRef group,
    AXUIElementRef el,
    uint64_t deadline_ns,
    fr_ax_hit_t *out_hit
);

#define FR_DOCK_EXEC_PATH "/System/Library/CoreServices/Dock.app/Contents/MacOS/Dock"
#define FR_FINDER_EXEC_PATH "/System/Library/CoreServices/Finder.app/Contents/MacOS/Finder"

// The window server's answer for a position: the owner of the window that a mouse-down there would hit (Dock and
// WindowServer windows do not count).
typedef enum {
    FR_WIN_UNKNOWN = 0, // No window was hit, or the owner could not be identified
    FR_WIN_FINDER = 1,
    FR_WIN_OTHER = 2,
} fr_window_owner_t;

// Asks only the window server and sends no AX messages to any app, so a stalled clicked app cannot slow it down.
fr_window_owner_t fr_top_window_owner(double x, double y);

// Process lookup, application elements and startup warm-up. A hit on an application element stays within that process
// and does not check whether other apps cover it from above.
pid_t fr_dock_pid(void);
pid_t fr_finder_pid(void);
AXUIElementRef fr_ax_create_application(pid_t pid);
int fr_ax_copy_role(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    uint64_t deadline_ns,
    char *buf,
    size_t buf_len,
    bool *budget_exceeded
);
bool fr_ax_dock_item_center(
    AXUIElementRef system_ax,
    AXUIElementRef dock_app,
    const char *finder_app_path,
    uint64_t deadline_ns,
    double *out_x,
    double *out_y,
    int *out_ax_err,
    bool *budget_exceeded
);

// CGEventTap and dedicated-thread CFRunLoop lifecycle.
fr_tap_t *fr_tap_create(uintptr_t go_handle, bool listen_only);
void fr_tap_run(fr_tap_t *t);
bool fr_tap_set_enabled(fr_tap_t *t, bool enabled);
void fr_tap_stop(fr_tap_t *t);
void fr_tap_destroy(fr_tap_t *t);

// Bundle validation, directory metadata classification, Finder path lookup, asynchronous NSWorkspace opens and the
// main-thread RunLoop.
bool fr_inspect_bundle(
    const char *app_path,
    char *out_bundle_id,
    size_t bid_len,
    char *out_exec_path,
    size_t exec_len
);
fr_dir_status_t fr_classify_directory(
    const char *dir_path,
    uint64_t *out_dev,
    uint64_t *out_ino
);
void fr_finder_app_path(char *out_path, size_t out_len);
void fr_open_application_async(const char *app_path, uint64_t req_id);
void fr_open_directory_async(const char *dir_path, const char *app_path, uint64_t req_id);

void fr_main_loop_prepare(void);
void fr_main_loop_run(void);
void fr_main_loop_stop(void);

// Declarations of the bridge callbacks exported from Go.
extern int goOnTapEvent(
    uintptr_t handle,
    uint32_t event_type,
    double x,
    double y,
    int64_t click_count,
    uint64_t flags,
    uint64_t time_ns
);
extern void goOnWorkspaceComplete(
    uint64_t req_id,
    _Bool failed,
    int err_code,
    char *err_domain
);
extern void goOnSystemReset(void);

#endif // FINDRELAY_DARWIN_BRIDGE_H
