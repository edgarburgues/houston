//! Dev tool: render the real v2 UI (basics theme) headlessly to an HTML grid
//! for design/contrast review. Run:  cargo run --example shot -p houston-tui
fn main() {
    let html = format!(
        "<!doctype html><html lang=\"es\"><meta charset=\"utf-8\"><title>Houston — terminal sizes</title>\
         <style>body{{background:#17191d;color:#ddd;font:16px system-ui;margin:24px}}\
         figure{{margin:24px 0;overflow:auto}}figcaption{{margin-bottom:8px}}\
         pre{{font:14px/1.25 Consolas,monospace;display:inline-block;background:#0c0c0c;padding:8px;margin:0}}</style>\
         <h1>Houston — terminal sizes</h1>{}</html>",
        houston_tui::demo_screens_html()
    );
    if let Some(path) = std::env::args_os().nth(1) {
        std::fs::write(path, html).expect("write screenshot gallery");
    } else {
        print!("{html}");
    }
}
