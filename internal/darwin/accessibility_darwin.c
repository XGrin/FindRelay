#include "bridge.h"

#include <libproc.h>
#include <limits.h>
#include <stdio.h>
#include <string.h>
#include <time.h>

#define FR_DEFAULT_FINDER_APP "/System/Library/CoreServices/Finder.app"
#define FR_AX_MAX_MSG_TIMEOUT_SEC 0.10f
#define FR_MAX_DISPLAYS 16

uint64_t fr_monotonic_now_ns(void) {
    return clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
}

bool fr_ax_is_trusted(bool prompt) {
    const void *keys[] = {kAXTrustedCheckOptionPrompt};
    const void *values[] = {prompt ? kCFBooleanTrue : kCFBooleanFalse};
    CFDictionaryRef opts = CFDictionaryCreate(
        kCFAllocatorDefault,
        keys,
        values,
        1,
        &kCFTypeDictionaryKeyCallBacks,
        &kCFTypeDictionaryValueCallBacks
    );
    if (!opts) {
        return AXIsProcessTrusted();
    }
    bool trusted = AXIsProcessTrustedWithOptions(opts);
    CFRelease(opts);
    return trusted;
}

AXUIElementRef fr_ax_create_system_wide(void) {
    return AXUIElementCreateSystemWide();
}

void fr_ax_release(AXUIElementRef ref) {
    if (ref) {
        CFRelease(ref);
    }
}

static bool point_strictly_inside_any(
    double x,
    double y,
    const fr_rect_t *displays,
    uint32_t count
) {
    for (uint32_t i = 0; i < count; ++i) {
        double min_x = displays[i].x;
        double min_y = displays[i].y;
        double max_x = min_x + displays[i].width;
        double max_y = min_y + displays[i].height;
        if (displays[i].width > 0 && displays[i].height > 0 &&
            x >= min_x && x < max_x && y >= min_y && y < max_y) {
            return true;
        }
    }
    return false;
}

void fr_project_dock_edge_point(
    double x,
    double y,
    const fr_rect_t *displays,
    uint32_t display_count,
    double *out_x,
    double *out_y
) {
    *out_x = x;
    *out_y = y;
    if (!displays || display_count == 0) {
        return;
    }

    int chosen = -1;
    for (uint32_t i = 0; i < display_count; ++i) {
        if (displays[i].width <= 16.0 || displays[i].height <= 16.0) {
            continue;
        }
        double min_x = displays[i].x;
        double min_y = displays[i].y;
        double max_x = min_x + displays[i].width;
        double max_y = min_y + displays[i].height;
        if (x >= min_x && x <= max_x && y >= min_y && y <= max_y) {
            chosen = (int)i;
            if (x < max_x && y < max_y) {
                break;
            }
        }
    }

    if (chosen < 0) {
        return;
    }

    double min_x = displays[chosen].x;
    double min_y = displays[chosen].y;
    double max_x = min_x + displays[chosen].width;
    double max_y = min_y + displays[chosen].height;

    double adj_x = x;
    double adj_y = y;

    if (x <= min_x + 1.0 && !point_strictly_inside_any(min_x - 2.0, y, displays, display_count)) {
        adj_x = min_x + 8.0;
    } else if (x >= max_x - 1.0 && !point_strictly_inside_any(max_x + 2.0, y, displays, display_count)) {
        adj_x = max_x - 8.0;
    }

    if (y >= max_y - 1.0 && !point_strictly_inside_any(x, max_y + 2.0, displays, display_count)) {
        adj_y = max_y - 8.0;
    }

    *out_x = adj_x;
    *out_y = adj_y;
}

static CGPoint project_live_display_edge(CGPoint pt) {
    CGDirectDisplayID ids[FR_MAX_DISPLAYS];
    uint32_t count = 0;
    if (CGGetActiveDisplayList(FR_MAX_DISPLAYS, ids, &count) != kCGErrorSuccess || count == 0) {
        return pt;
    }
    fr_rect_t rects[FR_MAX_DISPLAYS];
    for (uint32_t i = 0; i < count; ++i) {
        CGRect b = CGDisplayBounds(ids[i]);
        rects[i].x = b.origin.x;
        rects[i].y = b.origin.y;
        rects[i].width = b.size.width;
        rects[i].height = b.size.height;
    }
    double ox = pt.x;
    double oy = pt.y;
    fr_project_dock_edge_point(pt.x, pt.y, rects, count, &ox, &oy);
    return CGPointMake(ox, oy);
}

static bool prepare_ax_call(AXUIElementRef system_ax, uint64_t deadline_ns, bool *budget_exceeded) {
    uint64_t now = fr_monotonic_now_ns();
    if (now >= deadline_ns) {
        *budget_exceeded = true;
        return false;
    }
    float rem = (float)(deadline_ns - now) / 1e9f;
    if (rem > FR_AX_MAX_MSG_TIMEOUT_SEC) {
        rem = FR_AX_MAX_MSG_TIMEOUT_SEC;
    }
    if (rem < 0.001f) {
        rem = 0.001f;
    }
    AXError err = AXUIElementSetMessagingTimeout(system_ax, rem);
    return err == kAXErrorSuccess;
}

// Reads one attribute and reports the AX error code; on success *out_val is a new reference that the caller releases.
static AXError copy_ax_attr_code(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    CFStringRef attr,
    uint64_t deadline_ns,
    bool *budget_exceeded,
    CFTypeRef *out_val
) {
    *out_val = NULL;
    if (!prepare_ax_call(system_ax, deadline_ns, budget_exceeded)) {
        return kAXErrorSuccess;
    }
    AXError err = AXUIElementCopyAttributeValue(el, attr, out_val);
    if (err != kAXErrorSuccess) {
        if (*out_val) {
            CFRelease(*out_val);
        }
        *out_val = NULL;
    }
    return err;
}

static CFTypeRef copy_ax_attr(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    CFStringRef attr,
    uint64_t deadline_ns,
    bool *budget_exceeded
) {
    CFTypeRef val = NULL;
    copy_ax_attr_code(system_ax, el, attr, deadline_ns, budget_exceeded, &val);
    return val;
}

static void copy_cfstring_ascii(CFTypeRef ref, char *buf, size_t buf_len) {
    if (!buf || buf_len == 0) {
        return;
    }
    buf[0] = '\0';
    if (!ref || CFGetTypeID(ref) != CFStringGetTypeID()) {
        return;
    }
    if (!CFStringGetCString((CFStringRef)ref, buf, (CFIndex)buf_len, kCFStringEncodingUTF8)) {
        buf[0] = '\0';
    }
}

static bool bundle_url_is_finder(CFURLRef url) {
    CFBundleRef bundle = CFBundleCreate(kCFAllocatorDefault, url);
    if (!bundle) {
        return false;
    }
    CFStringRef bid = CFBundleGetIdentifier(bundle);
    bool is_finder = bid && CFEqual(bid, CFSTR("com.apple.finder"));
    CFRelease(bundle);
    return is_finder;
}

// Normalizes an AXURL attribute value to a CFURL; when *release_url is true, the caller releases the returned URL.
static CFURLRef normalize_ax_url(CFTypeRef url_ref, bool *release_url) {
    *release_url = false;
    if (!url_ref) {
        return NULL;
    }

    CFTypeID tid = CFGetTypeID(url_ref);
    if (tid == CFURLGetTypeID()) {
        return (CFURLRef)url_ref;
    }
    if (tid != CFStringGetTypeID()) {
        return NULL;
    }

    CFURLRef url = NULL;
    CFStringRef str = (CFStringRef)url_ref;
    if (CFStringHasPrefix(str, CFSTR("/"))) {
        url = CFURLCreateWithFileSystemPath(kCFAllocatorDefault, str, kCFURLPOSIXPathStyle, true);
    } else {
        url = CFURLCreateWithString(kCFAllocatorDefault, str, NULL);
    }
    *release_url = (url != NULL);
    return url;
}

static fr_url_kind_t classify_ax_url(CFTypeRef url_ref, const char *finder_app_path) {
    bool release_url = false;
    CFURLRef url = normalize_ax_url(url_ref, &release_url);
    if (!url) {
        return FR_URL_NONE;
    }

    CFStringRef scheme = CFURLCopyScheme(url);
    bool is_file = scheme && CFStringCompare(scheme, CFSTR("file"), kCFCompareCaseInsensitive) == kCFCompareEqualTo;
    if (scheme) {
        CFRelease(scheme);
    }
    if (!is_file) {
        if (release_url) {
            CFRelease(url);
        }
        return FR_URL_NON_FILE;
    }

    char path_buf[PATH_MAX];
    if (!CFURLGetFileSystemRepresentation(url, true, (UInt8 *)path_buf, sizeof(path_buf))) {
        if (release_url) {
            CFRelease(url);
        }
        return FR_URL_FILE;
    }

    size_t n = strlen(path_buf);
    while (n > 1 && path_buf[n - 1] == '/') {
        path_buf[--n] = '\0';
    }

    if (strcmp(path_buf, finder_app_path) == 0 || strcmp(path_buf, FR_DEFAULT_FINDER_APP) == 0) {
        if (release_url) {
            CFRelease(url);
        }
        return FR_URL_FINDER_APP;
    }
    if (n > 4 && strcasecmp(path_buf + n - 4, ".app") == 0) {
        // If the Finder system path differs because of volume mapping, fall back to verifying the system
        // com.apple.finder identifier (only when the file name is Finder.app).
        const char *slash = strrchr(path_buf, '/');
        const char *base = slash ? slash + 1 : path_buf;
        if (strcasecmp(base, "Finder.app") == 0 &&
            strncmp(path_buf, "/System/", 8) == 0 &&
            bundle_url_is_finder(url)) {
            if (release_url) {
                CFRelease(url);
            }
            return FR_URL_FINDER_APP;
        }
        if (release_url) {
            CFRelease(url);
        }
        return FR_URL_OTHER_APP;
    }
    if (release_url) {
        CFRelease(url);
    }
    return FR_URL_FILE;
}

// Performs one bounded hit test on scope; when scope is the system-wide element it takes the topmost element by window
// layering, and when it is an application element it hits only within that app.
// On success returns a new reference (released by the caller); otherwise returns NULL, and *out_ax_err holds the final
// AX error code.
AXUIElementRef fr_ax_hit(
    AXUIElementRef system_ax,
    AXUIElementRef scope,
    double x,
    double y,
    uint64_t deadline_ns,
    int *out_ax_err,
    bool *budget_exceeded
) {
    *out_ax_err = 0;
    bool retried_cold = false;
    bool projected = false;
    CGPoint pt = CGPointMake(x, y);

    for (int attempt = 0; attempt < 3; ++attempt) {
        if (!prepare_ax_call(system_ax, deadline_ns, budget_exceeded)) {
            return NULL;
        }
        uint64_t call_start = fr_monotonic_now_ns();
        AXUIElementRef el = NULL;
        AXError err = AXUIElementCopyElementAtPosition(scope, (float)pt.x, (float)pt.y, &el);
        uint64_t call_elapsed = fr_monotonic_now_ns() - call_start;
        *out_ax_err = (int)err;
        if (err == kAXErrorSuccess && el != NULL) {
            return el;
        }
        if (el) {
            CFRelease(el);
        }
        // Only when the call returns quickly (< 25ms) with CannotComplete is it treated as Dock AX being transiently
        // not ready during cold start and retried once; if the single message timeout was already used up because the
        // target window's main thread is hung, do not start a second synchronous blocking wait.
        if (err == kAXErrorCannotComplete && !retried_cold && call_elapsed < UINT64_C(25000000)) {
            retried_cold = true;
            continue;
        }
        // When there is no value and the point lies on an outer display edge (Dock's AX edge gap), project it inward
        // and retry once.
        if (err == kAXErrorNoValue && !projected) {
            CGPoint adj = project_live_display_edge(pt);
            if (!CGPointEqualToPoint(pt, adj)) {
                pt = adj;
                projected = true;
                continue;
            }
        }
        break;
    }
    if (fr_monotonic_now_ns() >= deadline_ns) {
        *budget_exceeded = true;
    }
    return NULL;
}

bool fr_ax_elements_equal(AXUIElementRef a, AXUIElementRef b) {
    return a && b && CFEqual(a, b);
}

static void append_chain_entry(char *chain, size_t cap, const char *role, const char *subrole) {
    if (!role || role[0] == '\0') {
        return;
    }
    size_t used = strlen(chain);
    if (used >= cap - 1) {
        return;
    }
    const char *sep = used > 0 ? ">" : "";
    if (subrole && subrole[0] != '\0') {
        snprintf(chain + used, cap - used, "%s%s/%s", sep, role, subrole);
    } else {
        snprintf(chain + used, cap - used, "%s%s", sep, role);
    }
}

// ---- Finder native desktop container evidence ----

#define FR_AX_ROLE_APPLICATION "AXApplication"
#define FR_AX_ROLE_GROUP "AXGroup"

// Process generation: uses the process start time to tell apart successive instances with the same PID (for example
// after a Finder restart).
static uint64_t process_start_generation(pid_t pid) {
    struct proc_bsdinfo info;
    memset(&info, 0, sizeof(info));
    if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, (uint32_t)sizeof(info)) <= 0) {
        return 0;
    }
    return ((uint64_t)info.pbi_start_tvsec << 32) | ((uint64_t)info.pbi_start_tvusec & UINT64_C(0xFFFFFFFF));
}

// Extracts a local file path from an AXURL attribute value (trailing slashes removed); returns false for non-file URLs
// or paths that cannot be represented.
static bool copy_file_path_from_ax_url(CFTypeRef url_ref, char *buf, size_t buf_len) {
    if (!buf || buf_len == 0) {
        return false;
    }
    buf[0] = '\0';

    bool release_url = false;
    CFURLRef url = normalize_ax_url(url_ref, &release_url);
    if (!url) {
        return false;
    }

    bool ok = false;
    CFStringRef scheme = CFURLCopyScheme(url);
    bool is_file = scheme && CFStringCompare(scheme, CFSTR("file"), kCFCompareCaseInsensitive) == kCFCompareEqualTo;
    if (scheme) {
        CFRelease(scheme);
    }
    if (is_file) {
        UInt8 path_buf[PATH_MAX];
        if (CFURLGetFileSystemRepresentation(url, true, path_buf, sizeof(path_buf)) && path_buf[0] == '/') {
            size_t n = strlen((const char *)path_buf);
            while (n > 1 && path_buf[n - 1] == '/') {
                path_buf[--n] = '\0';
            }
            if (n > 1 && n < buf_len) {
                memcpy(buf, path_buf, n + 1);
                ok = true;
            }
        }
    }
    if (release_url) {
        CFRelease(url);
    }
    return ok;
}

// Reads the selection set of the desktop icon container and records whether the hit element is the only selected item
// (multiple selection excluded).
static void read_selection_info(
    AXUIElementRef system_ax,
    AXUIElementRef group,
    AXUIElementRef hit_el,
    uint64_t deadline_ns,
    fr_ax_hit_t *out_hit
) {
    CFTypeRef sel_ref = copy_ax_attr(system_ax, group, kAXSelectedChildrenAttribute, deadline_ns, &out_hit->budget_exceeded);
    if (!sel_ref) {
        return;
    }
    if (CFGetTypeID(sel_ref) == CFArrayGetTypeID()) {
        CFArrayRef selected = (CFArrayRef)sel_ref;
        CFIndex count = CFArrayGetCount(selected);
        out_hit->has_selection_info = true;
        out_hit->selection_count = (int32_t)count;
        if (count == 1) {
            const void *only = CFArrayGetValueAtIndex(selected, 0);
            if (only && CFGetTypeID((CFTypeRef)only) == AXUIElementGetTypeID()) {
                out_hit->is_single_selected_self = CFEqual((CFTypeRef)only, hit_el);
            }
        }
    }
    CFRelease(sel_ref);
}

// ---- Process lookup and application elements ----

// Finds a process ID by executable path; returns 0 if not found.
// The process table may grow between two calls: first ask the system how many bytes are needed, and if the buffer is
// still too small, query again with the returned size.
static pid_t find_process_pid(const char *exec_path) {
    int needed = proc_listpids(PROC_ALL_PIDS, 0, NULL, 0);
    if (needed <= 0) {
        return 0;
    }
    size_t cap = (size_t)needed + 256 * sizeof(pid_t);
    pid_t *pids = (pid_t *)malloc(cap);
    if (!pids) {
        return 0;
    }

    char path[PROC_PIDPATHINFO_MAXSIZE];
    pid_t found = 0;
    for (int attempt = 0; attempt < 2 && found == 0; ++attempt) {
        int written = proc_listpids(PROC_ALL_PIDS, 0, pids, (uint32_t)cap);
        if (written <= 0) {
            break;
        }
        if ((size_t)written > cap) {
            size_t grown_cap = (size_t)written + 256 * sizeof(pid_t);
            pid_t *grown = (pid_t *)realloc(pids, grown_cap);
            if (!grown) {
                break;
            }
            pids = grown;
            cap = grown_cap;
            continue;
        }
        size_t count = (size_t)written / sizeof(pid_t);
        for (size_t i = 0; i < count; ++i) {
            if (pids[i] <= 0) {
                continue;
            }
            memset(path, 0, sizeof(path));
            if (proc_pidpath(pids[i], path, sizeof(path)) > 0 && strcmp(path, exec_path) == 0) {
                found = pids[i];
                break;
            }
        }
    }
    free(pids);
    return found;
}

pid_t fr_dock_pid(void) {
    return find_process_pid(FR_DOCK_EXEC_PATH);
}

pid_t fr_finder_pid(void) {
    return find_process_pid(FR_FINDER_EXEC_PATH);
}

AXUIElementRef fr_ax_create_application(pid_t pid) {
    if (pid <= 0) {
        return NULL;
    }
    return AXUIElementCreateApplication(pid);
}

int fr_ax_copy_role(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    uint64_t deadline_ns,
    char *buf,
    size_t buf_len,
    bool *budget_exceeded
) {
    if (buf && buf_len > 0) {
        buf[0] = '\0';
    }
    CFTypeRef role_ref = NULL;
    AXError err = copy_ax_attr_code(system_ax, el, kAXRoleAttribute, deadline_ns, budget_exceeded, &role_ref);
    if (err == kAXErrorSuccess && role_ref) {
        copy_cfstring_ascii(role_ref, buf, buf_len);
        CFRelease(role_ref);
    }
    return (int)err;
}

// ---- Dock icon location (for startup warm-up) ----

// Element center point (from AXPosition and AXSize).
static bool element_center(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    uint64_t deadline_ns,
    bool *budget_exceeded,
    double *out_x,
    double *out_y
) {
    CGPoint pos = CGPointZero;
    CGSize size = CGSizeZero;
    CFTypeRef pos_ref = copy_ax_attr(system_ax, el, kAXPositionAttribute, deadline_ns, budget_exceeded);
    bool ok = pos_ref && CFGetTypeID(pos_ref) == AXValueGetTypeID() &&
              AXValueGetValue((AXValueRef)pos_ref, kAXValueCGPointType, &pos);
    if (pos_ref) {
        CFRelease(pos_ref);
    }
    if (!ok) {
        return false;
    }
    CFTypeRef size_ref = copy_ax_attr(system_ax, el, kAXSizeAttribute, deadline_ns, budget_exceeded);
    ok = size_ref && CFGetTypeID(size_ref) == AXValueGetTypeID() &&
         AXValueGetValue((AXValueRef)size_ref, kAXValueCGSizeType, &size) && size.width > 0 && size.height > 0;
    if (size_ref) {
        CFRelease(size_ref);
    }
    if (!ok) {
        return false;
    }
    *out_x = pos.x + size.width / 2.0;
    *out_y = pos.y + size.height / 2.0;
    return true;
}

// Reports whether a Dock element is Finder's application icon: role AXDockItem, subrole AXApplicationDockItem, and a
// URL pointing to Finder.
static bool dock_item_is_finder(
    AXUIElementRef system_ax,
    AXUIElementRef item,
    const char *finder_app_path,
    uint64_t deadline_ns,
    bool *budget_exceeded
) {
    char role_buf[64] = "";
    char subrole_buf[64] = "";
    CFTypeRef role_ref = copy_ax_attr(system_ax, item, kAXRoleAttribute, deadline_ns, budget_exceeded);
    if (role_ref) {
        copy_cfstring_ascii(role_ref, role_buf, sizeof(role_buf));
        CFRelease(role_ref);
    }
    if (*budget_exceeded || strcmp(role_buf, "AXDockItem") != 0) {
        return false;
    }
    CFTypeRef subrole_ref = copy_ax_attr(system_ax, item, kAXSubroleAttribute, deadline_ns, budget_exceeded);
    if (subrole_ref) {
        copy_cfstring_ascii(subrole_ref, subrole_buf, sizeof(subrole_buf));
        CFRelease(subrole_ref);
    }
    if (*budget_exceeded || strcmp(subrole_buf, "AXApplicationDockItem") != 0) {
        return false;
    }
    bool is_finder = false;
    CFTypeRef url_ref = copy_ax_attr(system_ax, item, kAXURLAttribute, deadline_ns, budget_exceeded);
    if (url_ref) {
        is_finder = classify_ax_url(url_ref, finder_app_path) == FR_URL_FINDER_APP;
        CFRelease(url_ref);
    }
    return is_finder;
}

bool fr_ax_dock_item_center(
    AXUIElementRef system_ax,
    AXUIElementRef dock_app,
    const char *finder_app_path,
    uint64_t deadline_ns,
    double *out_x,
    double *out_y,
    int *out_ax_err,
    bool *budget_exceeded
) {
    *out_ax_err = 0;
    *budget_exceeded = false;
    if (!system_ax || !dock_app) {
        *out_ax_err = (int)kAXErrorIllegalArgument;
        return false;
    }

    // Dock's AX tree: AXApplication contains AXList, and AXList contains the AXDockItem elements.
    CFTypeRef lists_ref = NULL;
    AXError err = copy_ax_attr_code(system_ax, dock_app, kAXChildrenAttribute, deadline_ns, budget_exceeded, &lists_ref);
    if (err != kAXErrorSuccess || !lists_ref) {
        *out_ax_err = (int)err;
        return false;
    }

    CFTypeID element_type = AXUIElementGetTypeID();
    bool found = false;
    if (CFGetTypeID(lists_ref) == CFArrayGetTypeID()) {
        CFArrayRef lists = (CFArrayRef)lists_ref;
        for (CFIndex i = 0; i < CFArrayGetCount(lists) && !found && !*budget_exceeded; ++i) {
            CFTypeRef list = CFArrayGetValueAtIndex(lists, i);
            if (CFGetTypeID(list) != element_type) {
                continue;
            }
            CFTypeRef items_ref = NULL;
            err = copy_ax_attr_code(system_ax, (AXUIElementRef)list, kAXChildrenAttribute, deadline_ns, budget_exceeded, &items_ref);
            if (!items_ref) {
                continue;
            }
            if (CFGetTypeID(items_ref) == CFArrayGetTypeID()) {
                CFArrayRef items = (CFArrayRef)items_ref;
                for (CFIndex j = 0; j < CFArrayGetCount(items) && !found && !*budget_exceeded; ++j) {
                    CFTypeRef item = CFArrayGetValueAtIndex(items, j);
                    if (CFGetTypeID(item) != element_type) {
                        continue;
                    }
                    if (!dock_item_is_finder(system_ax, (AXUIElementRef)item, finder_app_path, deadline_ns, budget_exceeded)) {
                        continue;
                    }
                    found = element_center(system_ax, (AXUIElementRef)item, deadline_ns, budget_exceeded, out_x, out_y);
                }
            }
            CFRelease(items_ref);
        }
    }
    CFRelease(lists_ref);
    if (!found && err != kAXErrorSuccess) {
        *out_ax_err = (int)err;
    }
    if (fr_monotonic_now_ns() >= deadline_ns) {
        *budget_exceeded = true;
    }
    return found;
}

// Reads the hit element's AXURL: classifies it, and extracts the path when it is a local file URL.
static void read_url_evidence(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    uint64_t deadline_ns,
    const char *finder_app_path,
    fr_ax_hit_t *out_hit
) {
    CFTypeRef url_ref = copy_ax_attr(system_ax, el, kAXURLAttribute, deadline_ns, &out_hit->budget_exceeded);
    if (!url_ref) {
        return;
    }
    out_hit->url_kind = classify_ax_url(url_ref, finder_app_path);
    if (out_hit->url_kind == FR_URL_FILE) {
        copy_file_path_from_ax_url(url_ref, out_hit->file_path, sizeof(out_hit->file_path));
    }
    CFRelease(url_ref);
}

// Inspects an already-hit element; el is owned by the caller and this function does not release it.
// Observe mode (collect_observe_chain) collects the full parent chain and each level's subrole. Active mode reads only
// the attributes needed for recognition: the Dock chain stops once it has read the icon's application URL; the Finder
// chain reads only roles and stops as soon as AXWindow appears in the parent chain, and the caller reads the URL and
// selection info separately after confirming the desktop icon container (fr_ax_read_desktop_evidence).
// When the second level of the active-mode Finder chain is an AXGroup, it is handed to the caller as a new reference
// (out_hit->parent_group, released by the caller).
void fr_ax_inspect(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    uint64_t deadline_ns,
    const char *finder_app_path,
    bool inspect_desktop,
    bool collect_observe_chain,
    fr_ax_hit_t *out_hit
) {
    if (!out_hit) {
        return;
    }
    memset(out_hit, 0, sizeof(*out_hit));
    if (!system_ax) {
        out_hit->ax_error = (int)kAXErrorAPIDisabled;
        return;
    }
    if (!el) {
        out_hit->ax_error = (int)kAXErrorFailure;
        return;
    }

    pid_t pid = 0;
    AXError pid_err = AXUIElementGetPid(el, &pid);
    if (pid_err != kAXErrorSuccess || pid <= 0) {
        out_hit->ax_error = (int)pid_err;
        if (fr_monotonic_now_ns() >= deadline_ns) {
            out_hit->budget_exceeded = true;
        }
        return;
    }
    out_hit->pid = (int32_t)pid;

    char exec_path[PROC_PIDPATHINFO_MAXSIZE] = "";
    int path_len = proc_pidpath(pid, exec_path, sizeof(exec_path));
    if (path_len > 0) {
        if (strcmp(exec_path, FR_DOCK_EXEC_PATH) == 0) {
            out_hit->proc_kind = FR_PROC_DOCK;
        } else if (strcmp(exec_path, FR_FINDER_EXEC_PATH) == 0) {
            out_hit->proc_kind = FR_PROC_FINDER;
        } else {
            out_hit->proc_kind = FR_PROC_OTHER;
        }
    }

    bool inspect_dock_chain = out_hit->proc_kind == FR_PROC_DOCK;
    bool inspect_finder_chain = (inspect_desktop || collect_observe_chain) && out_hit->proc_kind == FR_PROC_FINDER;
    // No attributes are read for other processes, which keeps ordinary events at zero AX cost.
    if (!inspect_dock_chain && !inspect_finder_chain) {
        if (fr_monotonic_now_ns() >= deadline_ns) {
            out_hit->budget_exceeded = true;
        }
        return;
    }
    if (inspect_finder_chain) {
        out_hit->proc_gen = process_start_generation(pid);
    }
    bool active_finder_chain = inspect_finder_chain && !collect_observe_chain;

    // cur is the element reference at the current level. Level 0 is owned by the caller, so retain it once first and
    // release each level during the walk.
    AXUIElementRef cur = (AXUIElementRef)CFRetain(el);

    for (int depth = 0; depth < FR_AX_MAX_DEPTH && cur != NULL; ++depth) {
        char role_buf[64] = "";
        char subrole_buf[64] = "";

        CFTypeRef role_ref = copy_ax_attr(system_ax, cur, kAXRoleAttribute, deadline_ns, &out_hit->budget_exceeded);
        if (role_ref) {
            copy_cfstring_ascii(role_ref, role_buf, sizeof(role_buf));
            CFRelease(role_ref);
        }
        if (out_hit->budget_exceeded) {
            break;
        }

        // Subroles are used only for diagnostics; the active-mode Finder chain does not read them.
        if (!active_finder_chain) {
            CFTypeRef subrole_ref = copy_ax_attr(system_ax, cur, kAXSubroleAttribute, deadline_ns, &out_hit->budget_exceeded);
            if (subrole_ref) {
                copy_cfstring_ascii(subrole_ref, subrole_buf, sizeof(subrole_buf));
                CFRelease(subrole_ref);
            }
        }

        if (depth == 0) {
            snprintf(out_hit->role, sizeof(out_hit->role), "%s", role_buf);
            snprintf(out_hit->subrole, sizeof(out_hit->subrole), "%s", subrole_buf);
        }
        if (collect_observe_chain) {
            append_chain_entry(out_hit->parent_chain, sizeof(out_hit->parent_chain), role_buf, subrole_buf);
        }
        snprintf(out_hit->roles[depth], FR_AX_ROLE_STORE, "%s", role_buf);
        out_hit->role_count = (int32_t)depth + 1;

        if (out_hit->budget_exceeded) {
            break;
        }

        if (out_hit->proc_kind == FR_PROC_DOCK && strcmp(role_buf, "AXDockItem") == 0) {
            snprintf(out_hit->role, sizeof(out_hit->role), "%s", role_buf);
            snprintf(out_hit->subrole, sizeof(out_hit->subrole), "%s", subrole_buf);
            if (strcmp(subrole_buf, "AXApplicationDockItem") == 0) {
                CFTypeRef url_ref = copy_ax_attr(system_ax, cur, kAXURLAttribute, deadline_ns, &out_hit->budget_exceeded);
                if (url_ref) {
                    out_hit->url_kind = classify_ax_url(url_ref, finder_app_path);
                    out_hit->is_dock_finder = (out_hit->url_kind == FR_URL_FINDER_APP);
                    CFRelease(url_ref);
                }
            }
            if (!collect_observe_chain) {
                break;
            }
        } else if (active_finder_chain) {
            if (depth == 0) {
                out_hit->element_hash = (uint64_t)CFHash(cur);
            } else if (depth == 1 && strcmp(role_buf, FR_AX_ROLE_GROUP) == 0) {
                out_hit->parent_group = (AXUIElementRef)CFRetain(cur);
            }
        } else if (inspect_finder_chain && depth == 0) {
            out_hit->element_hash = (uint64_t)CFHash(cur);
            read_url_evidence(system_ax, cur, deadline_ns, finder_app_path, out_hit);
        } else if (inspect_finder_chain && depth == 1 && strcmp(role_buf, FR_AX_ROLE_GROUP) == 0) {
            read_selection_info(system_ax, cur, el, deadline_ns, out_hit);
        } else if (collect_observe_chain && out_hit->url_kind == FR_URL_NONE) {
            CFTypeRef url_ref = copy_ax_attr(system_ax, cur, kAXURLAttribute, deadline_ns, &out_hit->budget_exceeded);
            if (url_ref) {
                out_hit->url_kind = classify_ax_url(url_ref, finder_app_path);
                CFRelease(url_ref);
            }
        }

        if (out_hit->budget_exceeded) {
            break;
        }

        // A hit inside a window cannot be a desktop icon: the active-mode Finder chain stops here.
        if (active_finder_chain && strcmp(role_buf, "AXWindow") == 0) {
            break;
        }
        if (strcmp(role_buf, FR_AX_ROLE_APPLICATION) == 0) {
            break;
        }

        CFTypeRef parent_ref = copy_ax_attr(system_ax, cur, kAXParentAttribute, deadline_ns, &out_hit->budget_exceeded);
        CFRelease(cur);
        cur = NULL;
        if (!parent_ref) {
            break;
        }
        if (CFGetTypeID(parent_ref) != AXUIElementGetTypeID()) {
            CFRelease(parent_ref);
            break;
        }
        cur = (AXUIElementRef)parent_ref;
    }

    if (cur) {
        CFRelease(cur);
    }
    if (fr_monotonic_now_ns() >= deadline_ns) {
        out_hit->budget_exceeded = true;
        out_hit->is_dock_finder = false;
        out_hit->file_path[0] = '\0';
        // The parent chain is incomplete when over budget and cannot be used to classify the container.
        out_hit->role_count = 0;
    }
}

// After confirming that the hit element belongs to the native desktop icon container, reads the evidence: the hit
// element's AXURL and the parent group's selection set, one query each.
void fr_ax_read_desktop_evidence(
    AXUIElementRef system_ax,
    AXUIElementRef el,
    AXUIElementRef group,
    uint64_t deadline_ns,
    const char *finder_app_path,
    fr_ax_hit_t *out_hit
) {
    read_url_evidence(system_ax, el, deadline_ns, finder_app_path, out_hit);
    if (!out_hit->budget_exceeded) {
        read_selection_info(system_ax, group, el, deadline_ns, out_hit);
    }
    if (fr_monotonic_now_ns() >= deadline_ns) {
        out_hit->budget_exceeded = true;
        out_hit->file_path[0] = '\0';
    }
}

// Reads the parent group's selection set (one query): refreshes the single-selection evidence when rechecking the
// second desktop click.
void fr_ax_read_selection(
    AXUIElementRef system_ax,
    AXUIElementRef group,
    AXUIElementRef el,
    uint64_t deadline_ns,
    fr_ax_hit_t *out_hit
) {
    read_selection_info(system_ax, group, el, deadline_ns, out_hit);
    if (fr_monotonic_now_ns() >= deadline_ns) {
        out_hit->budget_exceeded = true;
    }
}
