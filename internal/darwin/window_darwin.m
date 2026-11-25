// Note: cgo compiles Objective-C without ARC by default (it runs in manual reference counting, MRC, mode).
#import <AppKit/AppKit.h>
#import <dispatch/dispatch.h>
#import <libproc.h>
#import <string.h>

#include "bridge.h"

// Maximum number of layers to look down through while skipping Dock and WindowServer windows; beyond that the result is
// treated as undeterminable.
#define FR_WINDOW_HIT_MAX_DEPTH 8

static bool is_window_server_path(const char *path) {
    const char *slash = strrchr(path, '/');
    return strncmp(path, "/System/", 8) == 0 && slash && strcmp(slash + 1, "WindowServer") == 0;
}

static pid_t window_owner_pid(NSInteger window_number) {
    CGWindowID ids[1] = {(CGWindowID)window_number};
    CFArrayRef query = CFArrayCreate(kCFAllocatorDefault, (const void **)ids, 1, NULL);
    if (!query) {
        return 0;
    }
    CFArrayRef descriptions = CGWindowListCreateDescriptionFromArray(query);
    CFRelease(query);
    int pid = 0;
    if (descriptions && CFArrayGetCount(descriptions) > 0) {
        CFDictionaryRef window = (CFDictionaryRef)CFArrayGetValueAtIndex(descriptions, 0);
        CFTypeRef value = CFDictionaryGetValue(window, kCGWindowOwnerPID);
        if (!value || CFGetTypeID(value) != CFNumberGetTypeID() ||
            !CFNumberGetValue((CFNumberRef)value, kCFNumberIntType, &pid)) {
            pid = 0;
        }
    }
    if (descriptions) {
        CFRelease(descriptions);
    }
    return (pid_t)pid;
}

// windowNumberAtPoint only answers after the process has set up NSApplication; otherwise it always returns 0. The
// default activation policy of an unbundled executable is Prohibited, so no Dock icon or menu bar appears. In a new
// process this takes about 27 to 44 ms including the first query, and the startup warm-up pays for it.
static void ensure_appkit(void) {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        NSApplicationLoad();
    });
}

// The window list (CGWindowListCopyWindowInfo) only gives position and opacity and does not know whether a window
// accepts clicks: when Notification Center or Mission Control is open, it places a window covering the whole screen
// above the desktop, and the Control Center window is also larger than its panel; these windows only intercept clicks
// at the panel. windowNumberAtPoint answers with the window a mouse-down would hit, passing through the rest of such
// windows.
// Dock's full-screen transparent window and WindowServer's system layers do not mean an app is covering the point, so
// they are skipped and the search continues below; the caller checks Dock icons first with an in-app hit test on Dock.
fr_window_owner_t fr_top_window_owner(double x, double y) {
    ensure_appkit();
    @autoreleasepool {
        // Event coordinates have their origin at the top-left of the main display; AppKit screen coordinates have their
        // origin at the bottom-left of the main display.
        CGFloat primary_height = CGDisplayBounds(CGMainDisplayID()).size.height;
        NSPoint point = NSMakePoint(x, primary_height - y);
        NSInteger below = 0;
        for (int depth = 0; depth < FR_WINDOW_HIT_MAX_DEPTH; ++depth) {
            NSInteger window_number = [NSWindow windowNumberAtPoint:point belowWindowWithWindowNumber:below];
            if (window_number <= 0) {
                return FR_WIN_UNKNOWN;
            }
            pid_t pid = window_owner_pid(window_number);
            char path[PROC_PIDPATHINFO_MAXSIZE] = "";
            if (pid <= 0 || proc_pidpath(pid, path, sizeof(path)) <= 0) {
                return FR_WIN_UNKNOWN;
            }
            if (strcmp(path, FR_DOCK_EXEC_PATH) != 0 && !is_window_server_path(path)) {
                return strcmp(path, FR_FINDER_EXEC_PATH) == 0 ? FR_WIN_FINDER : FR_WIN_OTHER;
            }
            below = window_number;
        }
        return FR_WIN_UNKNOWN;
    }
}
