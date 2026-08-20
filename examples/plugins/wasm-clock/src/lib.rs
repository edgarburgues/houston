//! wasm-clock — a minimal compiled Houston 2.0 plugin (no_std, no deps).
//!
//! Implements the houston-plugins ABI by hand to show the shape a compiled
//! plugin has. A real plugin would pull a guest SDK + serde and read the
//! request; this one ignores the request and returns a fixed JSON Response, so
//! the whole thing stays tiny and dependency-free. Every language that targets
//! wasm32 can export these three symbols.

#![no_std]

use core::panic::PanicInfo;

#[panic_handler]
fn panic(_: &PanicInfo) -> ! {
    loop {}
}

/// A static arena we hand out regions from — no real allocator needed, and its
/// address is chosen by the linker (no collisions with data/stack).
static mut ARENA: [u8; 65536] = [0; 65536];
static mut OFF: usize = 0;

fn bump(n: usize) -> usize {
    unsafe {
        let base = core::ptr::addr_of_mut!(ARENA) as usize;
        let p = base + OFF;
        OFF += n;
        p
    }
}

/// The host asks for a writable region of `len` bytes (for the request).
#[no_mangle]
pub extern "C" fn houston_alloc(len: i32) -> i32 {
    bump(len as usize) as i32
}

const RESPONSE: &[u8] =
    br#"{"title":"wasm-clock","lines":[{"spans":[{"text":"  hello from a compiled plugin","bold":true}]},{"spans":[{"text":"  (this is real wasm)","dim":true}]}]}"#;

/// Handle the call and return a packed (ptr << 32 | len) pointing at the JSON
/// Response in linear memory.
#[no_mangle]
pub extern "C" fn houston_call(_ptr: i32, _len: i32) -> i64 {
    let n = RESPONSE.len();
    let dst = bump(n);
    unsafe {
        core::ptr::copy_nonoverlapping(RESPONSE.as_ptr(), dst as *mut u8, n);
    }
    ((dst as i64) << 32) | (n as i64)
}
