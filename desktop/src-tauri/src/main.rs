// Prevents an extra console window on Windows in release builds. Does nothing
// on other platforms.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    app_lib::run();
}
