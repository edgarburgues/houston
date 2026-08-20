//! Dev tool: render the real v2 UI (basics theme) headlessly to an HTML grid
//! for design/contrast review. Run:  cargo run --example shot -p houston-tui
fn main() {
    print!("{}", houston_tui::demo_screens_html());
}
