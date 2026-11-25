#include "bridge.h"

#include <pthread.h>
#include <stdatomic.h>
#include <stdlib.h>

// Off-screen release point: far from all displays, used to rewrite the paired mouse-up of a passed-through mouse-down
// into a drag-out release.
static const CGPoint FR_OFFSCREEN_POINT = {-32000.0, -32000.0};

struct fr_tap_s {
    uintptr_t go_handle;
    bool listen_only;
    CFMachPortRef port;
    CFRunLoopSourceRef source;
    CFRunLoopRef run_loop;
    pthread_mutex_t mu;
    atomic_bool stopped;
};

static CGEventRef fr_event_callback(
    CGEventTapProxy proxy,
    CGEventType type,
    CGEventRef event,
    void *user_info
) {
    fr_tap_t *t = (fr_tap_t *)user_info;
    // During an intentional shutdown, calling CGEventTapEnable(port, false) makes the system deliver
    // DisabledByUserInput; ignore it and pass the event through.
    if (!t || atomic_load(&t->stopped)) {
        return event;
    }

    uint64_t now_ns = fr_monotonic_now_ns();

    if (type == kCGEventTapDisabledByTimeout || type == kCGEventTapDisabledByUserInput) {
        (void)goOnTapEvent(t->go_handle, (uint32_t)type, 0, 0, 0, 0, now_ns);
        return event;
    }

    CGPoint loc = CGEventGetLocation(event);
    int64_t clicks = CGEventGetIntegerValueField(event, kCGMouseEventClickState);
    uint64_t flags = (uint64_t)CGEventGetFlags(event);

    int disp = goOnTapEvent(
        t->go_handle,
        (uint32_t)type,
        (double)loc.x,
        (double)loc.y,
        clicks,
        flags,
        now_ns
    );
    if (t->listen_only || disp == FR_TAP_PASS) {
        return event;
    }
    if (disp == FR_TAP_CANCEL_UP && type == kCGEventLeftMouseUp) {
        // Up-cancel mode: leave the real mouse cursor in place and move only the coordinates of the drag and up events
        // in this press's capture chain off-screen, so Dock receives a complete drag-out + release, which clears the
        // dimmed pressed state and cancels the long-press menu timer without opening Finder.
        CGPoint cancel_pt = FR_OFFSCREEN_POINT;
        CGEventRef drag_ev = CGEventCreateCopy(event);
        if (drag_ev) {
            CGEventSetType(drag_ev, kCGEventLeftMouseDragged);
            CGEventSetLocation(drag_ev, cancel_pt);
            CGEventSetIntegerValueField(drag_ev, kCGMouseEventDeltaX, -1000);
            CGEventSetIntegerValueField(drag_ev, kCGMouseEventDeltaY, -1000);
            CGEventSetIntegerValueField(drag_ev, kCGMouseEventWindowUnderMousePointer, 0);
            CGEventSetIntegerValueField(drag_ev, kCGMouseEventWindowUnderMousePointerThatCanHandleThisEvent, 0);
            CGEventTapPostEvent(proxy, drag_ev);
            CFRelease(drag_ev);
        }
        CGEventSetLocation(event, cancel_pt);
        CGEventSetIntegerValueField(event, kCGMouseEventWindowUnderMousePointer, 0);
        CGEventSetIntegerValueField(event, kCGMouseEventWindowUnderMousePointerThatCanHandleThisEvent, 0);
        return event;
    }
    if (disp == FR_TAP_CANCEL_DESKTOP_UP && type == kCGEventLeftMouseUp) {
        // Desktop double-click second-up cancel mode: inject no Dragged event (to avoid triggering a Finder
        // TDesktopView icon drag), and only move the paired mouse-up off-screen and clear click-state, so Finder ends
        // its mouse-down capture and the double-click and single-click rename gesture recognizers fail safely.
        CGPoint cancel_pt = FR_OFFSCREEN_POINT;
        CGEventSetLocation(event, cancel_pt);
        CGEventSetIntegerValueField(event, kCGMouseEventClickState, 0);
        CGEventSetIntegerValueField(event, kCGMouseEventWindowUnderMousePointer, 0);
        CGEventSetIntegerValueField(event, kCGMouseEventWindowUnderMousePointerThatCanHandleThisEvent, 0);
        return event;
    }
    return NULL;
}

fr_tap_t *fr_tap_create(uintptr_t go_handle, bool listen_only) {
    fr_tap_t *t = (fr_tap_t *)calloc(1, sizeof(fr_tap_t));
    if (!t) {
        return NULL;
    }
    t->go_handle = go_handle;
    t->listen_only = listen_only;
    atomic_init(&t->stopped, false);
    pthread_mutex_init(&t->mu, NULL);

    CGEventMask mask = CGEventMaskBit(kCGEventLeftMouseDown) |
                       CGEventMaskBit(kCGEventLeftMouseUp) |
                       CGEventMaskBit(kCGEventLeftMouseDragged);
    CGEventTapOptions opts = listen_only ? kCGEventTapOptionListenOnly : kCGEventTapOptionDefault;

    t->port = CGEventTapCreate(
        kCGSessionEventTap,
        kCGHeadInsertEventTap,
        opts,
        mask,
        fr_event_callback,
        t
    );
    if (!t->port) {
        pthread_mutex_destroy(&t->mu);
        free(t);
        return NULL;
    }

    t->source = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, t->port, 0);
    if (!t->source) {
        CFMachPortInvalidate(t->port);
        CFRelease(t->port);
        pthread_mutex_destroy(&t->mu);
        free(t);
        return NULL;
    }

    t->run_loop = (CFRunLoopRef)CFRetain(CFRunLoopGetCurrent());
    CFRunLoopAddSource(t->run_loop, t->source, kCFRunLoopCommonModes);
    CGEventTapEnable(t->port, true);
    return t;
}

void fr_tap_run(fr_tap_t *t) {
    if (!t || atomic_load(&t->stopped)) {
        return;
    }
    CFRunLoopRun();
}

bool fr_tap_set_enabled(fr_tap_t *t, bool enabled) {
    if (!t || atomic_load(&t->stopped)) {
        return false;
    }
    pthread_mutex_lock(&t->mu);
    if (atomic_load(&t->stopped) || !t->port) {
        pthread_mutex_unlock(&t->mu);
        return false;
    }
    CGEventTapEnable(t->port, enabled);
    bool is_enabled = CGEventTapIsEnabled(t->port);
    pthread_mutex_unlock(&t->mu);
    return is_enabled;
}

void fr_tap_stop(fr_tap_t *t) {
    if (!t) {
        return;
    }
    pthread_mutex_lock(&t->mu);
    if (atomic_exchange(&t->stopped, true)) {
        pthread_mutex_unlock(&t->mu);
        return;
    }
    if (t->port) {
        CGEventTapEnable(t->port, false);
    }
    CFRunLoopRef rl = t->run_loop ? (CFRunLoopRef)CFRetain(t->run_loop) : NULL;
    pthread_mutex_unlock(&t->mu);

    if (rl) {
        CFRunLoopPerformBlock(rl, kCFRunLoopCommonModes, ^{
            CFRunLoopStop(CFRunLoopGetCurrent());
        });
        CFRunLoopStop(rl);
        CFRunLoopWakeUp(rl);
        CFRelease(rl);
    }
}

void fr_tap_destroy(fr_tap_t *t) {
    if (!t) {
        return;
    }
    if (t->run_loop && t->source) {
        CFRunLoopRemoveSource(t->run_loop, t->source, kCFRunLoopCommonModes);
    }
    if (t->source) {
        CFRelease(t->source);
        t->source = NULL;
    }
    if (t->port) {
        CFMachPortInvalidate(t->port);
        CFRelease(t->port);
        t->port = NULL;
    }
    if (t->run_loop) {
        CFRelease(t->run_loop);
        t->run_loop = NULL;
    }
    pthread_mutex_destroy(&t->mu);
    free(t);
}
