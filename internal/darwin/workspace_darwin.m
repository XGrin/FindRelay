// Note: cgo compiles Objective-C without ARC by default (it runs in manual reference counting, MRC, mode).
// Any Objective-C object kept across an @autoreleasepool must call retain / release explicitly.
#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>
#import <Security/AuthSession.h>
#import <dispatch/dispatch.h>
#import <errno.h>
#import <fcntl.h>
#import <limits.h>
#import <pthread.h>
#import <stdatomic.h>
#import <stdlib.h>
#import <string.h>
#import <sys/stat.h>
#import <unistd.h>

#include "bridge.h"

#define FR_MAX_INFO_PLIST_BYTES (1024 * 1024)

int fr_current_session_info(uint32_t *out_session_id, bool *out_has_graphic_access) {
    SecuritySessionId sid = noSecuritySession;
    SessionAttributeBits attrs = 0;
    OSStatus status = SessionGetInfo(callerSecuritySession, &sid, &attrs);
    if (status != errSessionSuccess) {
        if (out_session_id) {
            *out_session_id = 0;
        }
        if (out_has_graphic_access) {
            *out_has_graphic_access = false;
        }
        return (int)status;
    }
    if (out_session_id) {
        *out_session_id = (uint32_t)sid;
    }
    if (out_has_graphic_access) {
        *out_has_graphic_access = (attrs & sessionHasGraphicAccess) != 0;
    }
    return 0;
}

static CFDataRef read_file_bounded(const char *path, size_t max_bytes) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        return NULL;
    }
    struct stat st;
    if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size <= 0 ||
        (uint64_t)st.st_size > (uint64_t)max_bytes) {
        close(fd);
        return NULL;
    }
    size_t size = (size_t)st.st_size;
    UInt8 *buf = (UInt8 *)malloc(size);
    if (!buf) {
        close(fd);
        return NULL;
    }
    size_t off = 0;
    while (off < size) {
        ssize_t n = read(fd, buf + off, size - off);
        if (n <= 0) {
            free(buf);
            close(fd);
            return NULL;
        }
        off += (size_t)n;
    }
    close(fd);
    CFDataRef data = CFDataCreate(kCFAllocatorDefault, buf, (CFIndex)size);
    free(buf);
    return data;
}

// Read and parse Contents/Info.plist directly (both binary and XML plists are supported) to avoid the permanent
// in-process cache of CFBundleCreate.
bool fr_inspect_bundle(
    const char *app_path,
    char *out_bundle_id,
    size_t bid_len,
    char *out_exec_path,
    size_t exec_len
) {
    if (out_bundle_id && bid_len > 0) {
        out_bundle_id[0] = '\0';
    }
    if (out_exec_path && exec_len > 0) {
        out_exec_path[0] = '\0';
    }
    if (!app_path || app_path[0] != '/') {
        return false;
    }

    struct stat app_st;
    if (stat(app_path, &app_st) != 0 || !S_ISDIR(app_st.st_mode)) {
        return false;
    }

    char plist_path[PATH_MAX];
    int n = snprintf(plist_path, sizeof(plist_path), "%s/Contents/Info.plist", app_path);
    if (n <= 0 || (size_t)n >= sizeof(plist_path)) {
        return false;
    }

    CFDataRef plist_data = read_file_bounded(plist_path, FR_MAX_INFO_PLIST_BYTES);
    if (!plist_data) {
        return false;
    }

    CFPropertyListRef plist = CFPropertyListCreateWithData(
        kCFAllocatorDefault,
        plist_data,
        kCFPropertyListImmutable,
        NULL,
        NULL
    );
    CFRelease(plist_data);
    if (!plist) {
        return false;
    }
    if (CFGetTypeID(plist) != CFDictionaryGetTypeID()) {
        CFRelease(plist);
        return false;
    }

    CFDictionaryRef dict = (CFDictionaryRef)plist;
    CFTypeRef bid_val = CFDictionaryGetValue(dict, CFSTR("CFBundleIdentifier"));
    if (bid_val && CFGetTypeID(bid_val) == CFStringGetTypeID() && out_bundle_id && bid_len > 0) {
        if (!CFStringGetCString((CFStringRef)bid_val, out_bundle_id, (CFIndex)bid_len, kCFStringEncodingUTF8)) {
            out_bundle_id[0] = '\0';
        }
    }

    bool ok = false;
    CFTypeRef exec_val = CFDictionaryGetValue(dict, CFSTR("CFBundleExecutable"));
    if (exec_val && CFGetTypeID(exec_val) == CFStringGetTypeID()) {
        char exec_name[NAME_MAX + 1] = "";
        if (CFStringGetCString((CFStringRef)exec_val, exec_name, (CFIndex)sizeof(exec_name), kCFStringEncodingUTF8) &&
            exec_name[0] != '\0' &&
            strchr(exec_name, '/') == NULL &&
            strcmp(exec_name, ".") != 0 &&
            strcmp(exec_name, "..") != 0) {
            char exec_buf[PATH_MAX];
            int en = snprintf(exec_buf, sizeof(exec_buf), "%s/Contents/MacOS/%s", app_path, exec_name);
            struct stat exec_st;
            if (en > 0 && (size_t)en < sizeof(exec_buf) &&
                stat(exec_buf, &exec_st) == 0 &&
                S_ISREG(exec_st.st_mode) &&
                access(exec_buf, X_OK) == 0) {
                if (out_exec_path && exec_len > 0) {
                    snprintf(out_exec_path, exec_len, "%s", exec_buf);
                }
                ok = true;
            }
        }
    }

    CFRelease(plist);
    return ok;
}

void fr_finder_app_path(char *out_path, size_t out_len) {
    if (!out_path || out_len == 0) {
        return;
    }
    @autoreleasepool {
        NSURL *url = [[NSWorkspace sharedWorkspace] URLForApplicationWithBundleIdentifier:@"com.apple.finder"];
        if (url != nil) {
            const char *rep = [url fileSystemRepresentation];
            if (rep != NULL && rep[0] == '/') {
                snprintf(out_path, out_len, "%s", rep);
                return;
            }
        }
        snprintf(out_path, out_len, "%s", "/System/Library/CoreServices/Finder.app");
    }
}

// Manual memory management: alloc/init holds one reference, which the caller's autoreleasepool releases exactly once.
// Do not autorelease an already-autoreleased convenience constructor result such as +configuration again; that is a
// double release and crashes when the pool drains.
static NSWorkspaceOpenConfiguration *fr_open_configuration(void) {
    NSWorkspaceOpenConfiguration *cfg = [[[NSWorkspaceOpenConfiguration alloc] init] autorelease];
    cfg.activates = YES;
    cfg.promptsUserIfNeeded = NO;
    return cfg;
}

// Each request delivers its result to Go at most once; late duplicate completions are dropped.
static void fr_deliver_workspace_result(uint64_t req_id, NSError *error, atomic_bool *delivered) {
    if (atomic_exchange(delivered, true)) {
        return;
    }
    @autoreleasepool {
        if (error != nil) {
            const char *dom = error.domain ? [error.domain UTF8String] : "NSError";
            goOnWorkspaceComplete(req_id, true, (int)error.code, (char *)dom);
        } else {
            goOnWorkspaceComplete(req_id, false, 0, "");
        }
    }
}

uint64_t fr_double_click_interval_ns(void) {
    static uint64_t cached_ns = 0;
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        cached_ns = (uint64_t)([NSEvent doubleClickInterval] * 1e9);
    });
    return cached_ns;
}

static bool fr_is_permission_error(NSError *error) {
    if ([error.domain isEqualToString:NSCocoaErrorDomain] &&
        (error.code == NSFileReadNoPermissionError || error.code == NSFileWriteNoPermissionError)) {
        return true;
    }
    NSError *underlying = error.userInfo[NSUnderlyingErrorKey];
    if ([underlying.domain isEqualToString:NSPOSIXErrorDomain] &&
        (underlying.code == EACCES || underlying.code == EPERM)) {
        return true;
    }
    return false;
}

// Returns 1 (true), 0 (false), or -1 (missing or unexpected type).
static int fr_bool_value(NSDictionary *values, NSURLResourceKey key) {
    NSNumber *number = [values objectForKey:key];
    if (![number isKindOfClass:[NSNumber class]]) {
        return -1;
    }
    return number.boolValue ? 1 : 0;
}

static fr_dir_status_t fr_check_directory_metadata(NSURL *url) {
    @autoreleasepool {
        NSArray *keys = @[
            NSURLIsDirectoryKey, NSURLIsSymbolicLinkKey, NSURLIsAliasFileKey, NSURLIsPackageKey,
            NSURLIsApplicationKey, NSURLIsVolumeKey, NSURLVolumeIsLocalKey, NSURLVolumeIsInternalKey,
            NSURLVolumeIsRemovableKey, NSURLVolumeIsEjectableKey, NSURLIsUbiquitousItemKey
        ];
        NSError *error = nil;
        NSDictionary *values = [url resourceValuesForKeys:keys error:&error];
        if (values == nil) {
            return fr_is_permission_error(error) ? FR_DIR_ERR_PERMISSION : FR_DIR_ERR_IO;
        }

        // If any of the following keys is true, the item belongs to a category that is not supported (keys and
        // statuses correspond by index).
        NSArray *reject_keys = @[
            NSURLIsSymbolicLinkKey, NSURLIsAliasFileKey, NSURLIsPackageKey, NSURLIsApplicationKey,
            NSURLIsVolumeKey, NSURLVolumeIsRemovableKey, NSURLVolumeIsEjectableKey, NSURLIsUbiquitousItemKey
        ];
        static const fr_dir_status_t reject_status[] = {
            FR_DIR_REJECT_SYMLINK, FR_DIR_REJECT_ALIAS, FR_DIR_REJECT_PACKAGE, FR_DIR_REJECT_PACKAGE,
            FR_DIR_REJECT_VOLUME, FR_DIR_REJECT_EXTERNAL_VOLUME, FR_DIR_REJECT_EXTERNAL_VOLUME,
            FR_DIR_REJECT_UBIQUITOUS
        };
        for (NSUInteger i = 0; i < [reject_keys count]; ++i) {
            if (fr_bool_value(values, reject_keys[i]) == 1) {
                return reject_status[i];
            }
        }

        // The following keys must be true; failing to read one means the category cannot be confirmed.
        NSArray *require_keys = @[NSURLIsDirectoryKey, NSURLVolumeIsLocalKey, NSURLVolumeIsInternalKey];
        static const fr_dir_status_t require_status[] = {
            FR_DIR_REJECT_NOT_DIRECTORY, FR_DIR_REJECT_NON_LOCAL_VOLUME, FR_DIR_REJECT_EXTERNAL_VOLUME
        };
        for (NSUInteger i = 0; i < [require_keys count]; ++i) {
            int value = fr_bool_value(values, require_keys[i]);
            if (value == -1) {
                return FR_DIR_REJECT_AMBIGUOUS_METADATA;
            }
            if (value == 0) {
                return require_status[i];
            }
        }
        return FR_DIR_ELIGIBLE;
    }
}

fr_dir_status_t fr_classify_directory(const char *dir_path, uint64_t *out_dev, uint64_t *out_ino) {
    if (out_dev) {
        *out_dev = 0;
    }
    if (out_ino) {
        *out_ino = 0;
    }
    if (!dir_path || dir_path[0] != '/') {
        return FR_DIR_REJECT_NOT_DIRECTORY;
    }

    // Trust the lstat result and do not follow symbolic links, so a link cannot pass validation by resolving to a
    // regular directory.
    struct stat st;
    if (lstat(dir_path, &st) != 0) {
        if (errno == EACCES || errno == EPERM) {
            return FR_DIR_ERR_PERMISSION;
        }
        if (errno == ENOENT || errno == ENOTDIR) {
            return FR_DIR_REJECT_NOT_FOUND;
        }
        return FR_DIR_ERR_IO;
    }
    if (S_ISLNK(st.st_mode)) {
        return FR_DIR_REJECT_SYMLINK;
    }
    if (!S_ISDIR(st.st_mode)) {
        return FR_DIR_REJECT_NOT_DIRECTORY;
    }

    @autoreleasepool {
        NSString *nsPath = [NSString stringWithUTF8String:dir_path];
        if (nsPath == nil) {
            return FR_DIR_ERR_IO;
        }
        NSURL *url = [NSURL fileURLWithPath:nsPath isDirectory:YES];
        fr_dir_status_t status = fr_check_directory_metadata(url);
        if (status != FR_DIR_ELIGIBLE) {
            return status;
        }
        // Only eligible directories return a file identity, which the executor uses to re-verify the same object before
        // opening it.
        if (out_dev) {
            *out_dev = (uint64_t)st.st_dev;
        }
        if (out_ino) {
            *out_ino = (uint64_t)st.st_ino;
        }
        return FR_DIR_ELIGIBLE;
    }
}

void fr_open_application_async(const char *app_path, uint64_t req_id) {
    if (!app_path || app_path[0] != '/') {
        goOnWorkspaceComplete(req_id, true, -1, "invalid_path");
        return;
    }
    @autoreleasepool {
        NSString *nsPath = [NSString stringWithUTF8String:app_path];
        if (nsPath == nil) {
            goOnWorkspaceComplete(req_id, true, -1, "invalid_utf8");
            return;
        }
        dispatch_async(dispatch_get_main_queue(), ^{
            @autoreleasepool {
                NSURL *appURL = [NSURL fileURLWithPath:nsPath isDirectory:YES];

                __block atomic_bool delivered = false;
                [[NSWorkspace sharedWorkspace] openApplicationAtURL:appURL
                                                      configuration:fr_open_configuration()
                                                  completionHandler:^(NSRunningApplication *app, NSError *error) {
                    (void)app;
                    fr_deliver_workspace_result(req_id, error, &delivered);
                }];
            }
        });
    }
}

void fr_open_directory_async(const char *dir_path, const char *app_path, uint64_t req_id) {
    if (!dir_path || dir_path[0] != '/' || !app_path || app_path[0] != '/') {
        goOnWorkspaceComplete(req_id, true, -1, "invalid_path");
        return;
    }
    @autoreleasepool {
        NSString *nsDir = [NSString stringWithUTF8String:dir_path];
        NSString *nsApp = [NSString stringWithUTF8String:app_path];
        if (nsDir == nil || nsApp == nil) {
            goOnWorkspaceComplete(req_id, true, -1, "invalid_utf8");
            return;
        }
        dispatch_async(dispatch_get_main_queue(), ^{
            @autoreleasepool {
                NSURL *dirURL = [NSURL fileURLWithPath:nsDir isDirectory:YES];
                NSURL *appURL = [NSURL fileURLWithPath:nsApp isDirectory:YES];

                __block atomic_bool delivered = false;
                [[NSWorkspace sharedWorkspace] openURLs:@[dirURL]
                                      withApplicationAtURL:appURL
                                              configuration:fr_open_configuration()
                                          completionHandler:^(NSRunningApplication *app, NSError *error) {
                    (void)app;
                    fr_deliver_workspace_result(req_id, error, &delivered);
                }];
            }
        });
    }
}

// Main-thread RunLoop and sleep/wake and display reconfiguration observers.
static pthread_mutex_t g_main_mu = PTHREAD_MUTEX_INITIALIZER;
static CFRunLoopSourceRef g_keepalive_source = NULL;
static id g_wake_observer = nil;
static bool g_display_cb_registered = false;
static bool g_main_running = false;
static bool g_main_stopped = false;

static void fr_noop_perform(void *info) {
    (void)info;
}

static void fr_display_reconfig_cb(
    CGDirectDisplayID display,
    CGDisplayChangeSummaryFlags flags,
    void *user_info
) {
    (void)display;
    (void)user_info;
    if ((flags & kCGDisplayBeginConfigurationFlag) == 0) {
        goOnSystemReset();
    }
}

// The caller must hold g_main_mu.
static void fr_main_loop_cleanup_locked(void) {
    if (g_display_cb_registered) {
        CGDisplayRemoveReconfigurationCallback(fr_display_reconfig_cb, NULL);
        g_display_cb_registered = false;
    }
    @autoreleasepool {
        if (g_wake_observer != nil) {
            [[[NSWorkspace sharedWorkspace] notificationCenter] removeObserver:g_wake_observer];
            [g_wake_observer release];
            g_wake_observer = nil;
        }
    }
    if (g_keepalive_source) {
        CFRunLoopRemoveSource(CFRunLoopGetMain(), g_keepalive_source, kCFRunLoopCommonModes);
        CFRelease(g_keepalive_source);
        g_keepalive_source = NULL;
    }
}

void fr_main_loop_prepare(void) {
    pthread_mutex_lock(&g_main_mu);
    g_main_stopped = false;
    if (!g_keepalive_source) {
        CFRunLoopSourceContext ctx = {0};
        ctx.perform = fr_noop_perform;
        g_keepalive_source = CFRunLoopSourceCreate(kCFAllocatorDefault, 0, &ctx);
        if (g_keepalive_source) {
            CFRunLoopAddSource(CFRunLoopGetMain(), g_keepalive_source, kCFRunLoopCommonModes);
        }
    }
    @autoreleasepool {
        if (g_wake_observer == nil) {
            NSNotificationCenter *nc = [[NSWorkspace sharedWorkspace] notificationCenter];
            id obs = [nc addObserverForName:NSWorkspaceDidWakeNotification
                                     object:nil
                                      queue:nil
                                 usingBlock:^(NSNotification *note) {
                (void)note;
                goOnSystemReset();
            }];
            g_wake_observer = [obs retain];
        }
    }
    if (!g_display_cb_registered) {
        if (CGDisplayRegisterReconfigurationCallback(fr_display_reconfig_cb, NULL) == kCGErrorSuccess) {
            g_display_cb_registered = true;
        }
    }
    pthread_mutex_unlock(&g_main_mu);
}

void fr_main_loop_run(void) {
    pthread_mutex_lock(&g_main_mu);
    if (g_main_stopped) {
        fr_main_loop_cleanup_locked();
        pthread_mutex_unlock(&g_main_mu);
        return;
    }
    g_main_running = true;
    pthread_mutex_unlock(&g_main_mu);

    CFRunLoopRun();

    pthread_mutex_lock(&g_main_mu);
    g_main_running = false;
    fr_main_loop_cleanup_locked();
    pthread_mutex_unlock(&g_main_mu);
}

void fr_main_loop_stop(void) {
    pthread_mutex_lock(&g_main_mu);
    g_main_stopped = true;
    bool running = g_main_running;
    if (!running) {
        // If fr_main_loop_run has not been entered yet (for example startTap failed and aborted), clean up the
        // registered resources immediately and do not dispatch leftover blocks.
        fr_main_loop_cleanup_locked();
        pthread_mutex_unlock(&g_main_mu);
        return;
    }
    pthread_mutex_unlock(&g_main_mu);

    CFRunLoopRef main_rl = CFRunLoopGetMain();
    CFRunLoopPerformBlock(main_rl, kCFRunLoopCommonModes, ^{
        pthread_mutex_lock(&g_main_mu);
        bool should_stop = g_main_stopped && g_main_running;
        pthread_mutex_unlock(&g_main_mu);
        if (should_stop) {
            CFRunLoopStop(CFRunLoopGetCurrent());
        }
    });
    CFRunLoopStop(main_rl);
    CFRunLoopWakeUp(main_rl);
}
