//go:build desktop

package main

/*
#cgo pkg-config: x11 gdk-pixbuf-2.0
#include <X11/Xlib.h>
#include <X11/Xatom.h>
#include <X11/Xutil.h>
#include <gdk-pixbuf/gdk-pixbuf.h>
#include <stdlib.h>
#include <string.h>

// Pre-encoded _NET_WM_ICON CARDINAL array passed in from Go side.
// X11 long is 64-bit on amd64, but the protocol wire format for
// CARDINAL[32] still passes one cardinal per 8 bytes via XLib's
// `unsigned long *` API — the server unpacks to 32-bit at the wire.
// Just pass the array straight through.
//
// Returns 0 on success, non-zero on failure with an integer status:
//   1 — XOpenDisplay failed
//   2 — no window matched WM_CLASS
//   3 — XChangeProperty errored (it's actually a void, but we keep
//       the slot for future error handling)
//
// Match logic walks _NET_CLIENT_LIST on the root window and inspects
// each child's WM_CLASS, looking for the class string "Iterion" (the
// StartupWMClass-equivalent stamped by the Wails GTK runtime via
// Linux.ProgramName="iterion"; GTK auto-capitalises the first letter
// for the WM_CLASS class field). On multiple matches we set on each
// — covers the rare case where a secondary window inherits the same
// WM_CLASS.
static int install_iterion_icon(unsigned long *data, unsigned long count) {
	Display *dpy = XOpenDisplay(NULL);
	if (!dpy) return 1;
	Atom net_client_list = XInternAtom(dpy, "_NET_CLIENT_LIST", False);
	Atom net_wm_icon     = XInternAtom(dpy, "_NET_WM_ICON",     False);

	Window root = DefaultRootWindow(dpy);
	Atom actual_type;
	int actual_format;
	unsigned long nitems = 0, bytes_after = 0;
	unsigned char *prop = NULL;
	int status = XGetWindowProperty(dpy, root, net_client_list, 0, (~0L), False,
		XA_WINDOW, &actual_type, &actual_format, &nitems, &bytes_after, &prop);
	if (status != Success || !prop) { XCloseDisplay(dpy); return 2; }

	Window *clients = (Window *)prop;
	int found = 0;
	for (unsigned long i = 0; i < nitems; i++) {
		XClassHint hint;
		if (XGetClassHint(dpy, clients[i], &hint) == 0) continue;
		int match = (hint.res_class != NULL && strcmp(hint.res_class, "Iterion") == 0);
		if (hint.res_name)  XFree(hint.res_name);
		if (hint.res_class) XFree(hint.res_class);
		if (!match) continue;
		XChangeProperty(dpy, clients[i], net_wm_icon, XA_CARDINAL, 32,
			PropModeReplace, (unsigned char *)data, (int)count);
		found++;
	}
	XFree(prop);
	XFlush(dpy);
	XCloseDisplay(dpy);
	return found > 0 ? 0 : 2;
}

// pack_icon_arglists decodes the embedded PNG bytes into a multi-size
// CARDINAL[2 + W*H, 2 + W*H, ...] block suitable for _NET_WM_ICON.
// The caller owns the returned buffer and must free it.
//
// Resizing is done via GdkPixbuf's bilinear scaler so the binary
// stays free of a third-party Go image resampler. Sizes chosen to
// match what Cinnamon's window matcher actually requests.
static unsigned long* pack_icon_argblock(const unsigned char *png_bytes, int png_len, unsigned long *out_count) {
	GError *err = NULL;
	GdkPixbufLoader *loader = gdk_pixbuf_loader_new();
	if (!gdk_pixbuf_loader_write(loader, png_bytes, png_len, &err) ||
	    !gdk_pixbuf_loader_close(loader, &err)) {
		if (err) g_error_free(err);
		g_object_unref(loader);
		*out_count = 0;
		return NULL;
	}
	GdkPixbuf *src = gdk_pixbuf_loader_get_pixbuf(loader);
	if (!src) { g_object_unref(loader); *out_count = 0; return NULL; }
	g_object_ref(src);
	g_object_unref(loader);

	int sizes[] = {16, 24, 32, 48, 64, 128, 256};
	int n_sizes = sizeof(sizes) / sizeof(sizes[0]);

	// Pre-compute total length.
	unsigned long total = 0;
	for (int i = 0; i < n_sizes; i++) total += 2 + (unsigned long)sizes[i] * sizes[i];
	unsigned long *buf = (unsigned long *)g_malloc0(total * sizeof(unsigned long));
	if (!buf) { g_object_unref(src); *out_count = 0; return NULL; }

	unsigned long off = 0;
	for (int i = 0; i < n_sizes; i++) {
		int sz = sizes[i];
		GdkPixbuf *scaled = gdk_pixbuf_scale_simple(src, sz, sz, GDK_INTERP_BILINEAR);
		if (!scaled) continue;
		GdkPixbuf *rgba = gdk_pixbuf_get_has_alpha(scaled) ? g_object_ref(scaled) : gdk_pixbuf_add_alpha(scaled, FALSE, 0, 0, 0);
		guchar *px = gdk_pixbuf_get_pixels(rgba);
		int stride = gdk_pixbuf_get_rowstride(rgba);
		buf[off++] = (unsigned long)sz;
		buf[off++] = (unsigned long)sz;
		for (int y = 0; y < sz; y++) {
			for (int x = 0; x < sz; x++) {
				guchar *p = px + y * stride + x * 4;
				unsigned long argb =
					((unsigned long)p[3] << 24) |
					((unsigned long)p[0] << 16) |
					((unsigned long)p[1] <<  8) |
					((unsigned long)p[2]);
				buf[off++] = argb;
			}
		}
		g_object_unref(rgba);
		g_object_unref(scaled);
	}
	g_object_unref(src);
	*out_count = off;
	return buf;
}
*/
import "C"

import (
	"time"
	"unsafe"
)

// installWindowIcon is invoked from app.onDomReady on Linux. It
// posts a fresh _NET_WM_ICON property on every X window whose WM_CLASS
// matches "Iterion" (the Wails runtime's StartupWMClass), so
// the WM stops falling back to the StartupWMClass → .desktop → hicolor
// lookup that Cinnamon's matcher gets wrong post the 2026-05-14
// upstream upgrade cascade.
//
// Wails already calls gtk_window_set_icon at window creation, which
// sets WM_HINTS.icon_pixmap; the EWMH atom (_NET_WM_ICON) is what
// Cinnamon actually reads, and GTK3 + WebKitGTK doesn't propagate to
// it in this configuration. This helper fills that gap directly via
// XChangeProperty.
//
// Runs asynchronously and retries a few times on cold-start to absorb
// the race between OnDomReady firing and the X window appearing in
// _NET_CLIENT_LIST. Failures are silent — the icon is a cosmetic
// nice-to-have, not load-bearing.
func installWindowIcon() {
	go func() {
		if len(appIcon) == 0 {
			return
		}
		var n C.ulong
		buf := C.pack_icon_argblock(
			(*C.uchar)(unsafe.Pointer(&appIcon[0])),
			C.int(len(appIcon)),
			&n,
		)
		if buf == nil || n == 0 {
			return
		}
		defer C.g_free(C.gpointer(buf))
		// Retry up to ~3s in case OnDomReady fires before the X
		// window is registered in _NET_CLIENT_LIST. Cinnamon often
		// takes a beat to map the new window after WebKitGTK realises.
		for i := 0; i < 12; i++ {
			rc := C.install_iterion_icon(buf, n)
			if rc == 0 {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()
}
