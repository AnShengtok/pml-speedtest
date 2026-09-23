#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

//! 打流测试 · Tauri 外壳
//! 职责：拉起 Go 引擎 sidecar -> 等端口就绪 -> 开一个指向本地引擎的原生窗口 -> 退出时回收引擎。
//! 取值/调度全部留在 Go 引擎里，本文件不做任何测速逻辑，保证与 PC 版、网页版同源。
//!
//! 端口发现：壳在自己的配置目录里生成独占的 engine.port 路径，通过 PML_PORT_FILE 传给引擎；
//! 这样即使同时开着 PC 版（它写 %APPDATA% 打流测试 目录），两边也不会互相覆盖。

use std::fs::OpenOptions;
use std::io::{Read, Write};
use std::net::TcpStream;
use std::path::Path;
use std::sync::Mutex;
use std::thread;
use std::time::{Duration, Instant};

use tauri::path::BaseDirectory;
use tauri::{Manager, RunEvent, Url, WebviewUrl, WebviewWindowBuilder};

/// DWM 窗口属性号。深色标题栏在 Win10 1809 起为 20，更早为 19；
/// 标题栏/边框/文字配色仅 Win11 支持，在 Win10 上调用失败并被忽略。
#[cfg(windows)]
const DWMWA_USE_IMMERSIVE_DARK_MODE: u32 = 20;
#[cfg(windows)]
const DWMWA_USE_IMMERSIVE_DARK_MODE_OLD: u32 = 19;
#[cfg(windows)]
const DWMWA_BORDER_COLOR: u32 = 34;
#[cfg(windows)]
const DWMWA_CAPTION_COLOR: u32 = 35;
#[cfg(windows)]
const DWMWA_TEXT_COLOR: u32 = 36;

#[cfg(windows)]
#[link(name = "dwmapi")]
extern "system" {
    fn DwmSetWindowAttribute(
        hwnd: *mut core::ffi::c_void,
        attr: u32,
        data: *const u32,
        size: u32,
    ) -> i32;
}

/// 让原生标题栏与页面深色主体一致：先强制深色（Win10/Win11 通用，与 PC 版 Go 壳用同一套属性），
/// 再把标题栏与边框染成页面背景 #070b16、标题文字染成页面前景 #eef2fb（仅 Win11 生效）。
#[cfg(windows)]
fn tint_titlebar(window: &tauri::WebviewWindow) {
    // COLORREF 字节序为 0x00BBGGRR。
    const BG: u32 = 0x0016_0B07; // #070b16
    const FG: u32 = 0x00FB_F2EE; // #eef2fb
    const SYS_DEFAULT: u32 = 0xFFFF_FFFE; // 染不上时交回系统默认
    let hwnd: *mut core::ffi::c_void = match window.hwnd() {
        Ok(h) => unsafe { core::mem::transmute(h) },
        Err(_) => return,
    };
    unsafe {
        let on: u32 = 1;
        DwmSetWindowAttribute(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE, &on, 4);
        DwmSetWindowAttribute(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE_OLD, &on, 4);
        for (attr, color) in [
            (DWMWA_CAPTION_COLOR, BG),
            (DWMWA_BORDER_COLOR, BG),
            (DWMWA_TEXT_COLOR, FG),
        ] {
            if DwmSetWindowAttribute(hwnd, attr, &color, 4) != 0 {
                DwmSetWindowAttribute(hwnd, attr, &SYS_DEFAULT, 4);
            }
        }
    }
}

#[cfg(not(windows))]
fn tint_titlebar(_window: &tauri::WebviewWindow) {}
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;

/// 引擎端口避让区间的首个端口，只在端口文件读不到时作为兜底。
const ENGINE_PORT_FALLBACK: u16 = 8799;
/// 等引擎写出端口文件的上限时间。
const PORT_WAIT: Duration = Duration::from_secs(20);
/// 等端口可连接的上限时间（端口文件先出现，服务随后就绪）。
const LISTEN_WAIT: Duration = Duration::from_secs(10);

fn append_log(path: &Path, bytes: &[u8]) {
    if let Ok(mut f) = OpenOptions::new().create(true).append(true).open(path) {
        let _ = f.write_all(bytes);
    }
}

/// 轮询端口文件，拿到引擎实际监听的端口。
fn read_port(port_file: &Path) -> Option<u16> {
    let deadline = Instant::now() + PORT_WAIT;
    while Instant::now() < deadline {
        if let Ok(txt) = std::fs::read_to_string(port_file) {
            if let Ok(p) = txt.trim().parse::<u16>() {
                return Some(p);
            }
        }
        thread::sleep(Duration::from_millis(100));
    }
    None
}

/// TCP 探测引擎是否已经开始监听。
fn wait_for_engine(port: u16) -> bool {
    let deadline = Instant::now() + LISTEN_WAIT;
    while Instant::now() < deadline {
        if TcpStream::connect(("127.0.0.1", port)).is_ok() {
            return true;
        }
        thread::sleep(Duration::from_millis(150));
    }
    false
}

/// 自绘标题栏的控制端口。与引擎端口区间（8799 起）错开，只监听本机回环。
const SHELL_CTRL_PORT: u16 = 8798;

/// 起一个只监听 127.0.0.1 的极简 HTTP 服务，接收页面顶栏发来的窗口指令。
/// 之所以走这条通道而不是 Tauri 的 remote IPC：capability 里的 remote 端口是写死的，
/// 而引擎端口是动态避让的，两者对不上；走本机回环也不用改动 Go 引擎。
fn spawn_window_control(app: tauri::AppHandle) -> u16 {
    let Ok(listener) = std::net::TcpListener::bind(("127.0.0.1", SHELL_CTRL_PORT)) else {
        return 0; // 端口被占：交回原生标题栏，功能不受影响
    };
    let port = listener.local_addr().map(|a| a.port()).unwrap_or(0);
    thread::spawn(move || {
        for stream in listener.incoming() {
            let Ok(mut stream) = stream else { continue };
            let mut buf = [0u8; 256];
            let Ok(size) = stream.read(&mut buf) else { continue };
            // 只需要请求行里的路径，不解析完整 HTTP 头。
            let head = String::from_utf8_lossy(&buf[..size]);
            // 页面用 ?t= 时间戳防缓存，这里要把查询串剥掉再匹配指令。
            let op = head
                .split_whitespace()
                .nth(1)
                .unwrap_or_default()
                .trim_start_matches('/')
                .split('?')
                .next()
                .unwrap_or_default()
                .to_owned();
            let _ = stream.write_all(
                b"HTTP/1.1 204 No Content\r\nAccess-Control-Allow-Origin: *\r\nConnection: close\r\n\r\n",
            );
            let app_inner = app.clone();
            let _ = app.run_on_main_thread(move || {
                let Some(window) = app_inner.get_webview_window("main") else {
                    return;
                };
                match op.as_str() {
                    "min" => {
                        let _ = window.minimize();
                    }
                    "max" => {
                        if window.is_maximized().unwrap_or(false) {
                            let _ = window.unmaximize();
                        } else {
                            let _ = window.maximize();
                        }
                    }
                    "drag" => {
                        let _ = window.start_dragging();
                    }
                    "close" => {
                        let _ = window.close();
                    }
                    _ => {}
                }
            });
        }
    });
    port
}

/// 引擎子进程状态，退出时用它杀掉 sidecar，不留孤儿进程。
struct Engine(Mutex<Option<CommandChild>>);

fn main() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .setup(|app| {
            let handle = app.handle().clone();

            let dir = handle
                .path()
                .resolve("pml", BaseDirectory::AppConfig)
                .unwrap_or_else(|_| std::env::temp_dir());
            let _ = std::fs::create_dir_all(&dir);
            let port_file = dir.join("engine.port");
            let log_file = dir.join("engine.log");
            // 先删掉上一次的端口文件，保证读到的端口一定是这次引擎新写的。
            let _ = std::fs::remove_file(&port_file);

            let builder = handle
                .shell()
                .sidecar("pml-engine")
                .map_err(|e| format!("sidecar 配置错误：{e}"))?;
            let (rx, child) = builder
                .envs([
                    ("PML_PORT_FILE", port_file.to_string_lossy().into_owned()),
                    ("PML_LOG", log_file.to_string_lossy().into_owned()),
                ])
                .spawn()
                .map_err(|e| format!("引擎启动失败：{e}"))?;
            handle.manage(Engine(Mutex::new(Some(child))));

            // 后台抽干 sidecar 输出，避免管道写满把引擎堵死（引擎日志本身走 PML_LOG 落盘）。
            let rx_log = log_file.clone();
            tauri::async_runtime::spawn(async move {
                let mut rx = rx;
                let log_path = rx_log;
                while let Some(ev) = rx.recv().await {
                    match ev {
                        CommandEvent::Stdout(b) | CommandEvent::Stderr(b) => {
                            append_log(&log_path, &b)
                        }
                        CommandEvent::Error(msg) => append_log(&log_path, msg.as_bytes()),
                        _ => {}
                    }
                }
            });

            let port = read_port(&port_file).unwrap_or(ENGINE_PORT_FALLBACK);
            if !wait_for_engine(port) {
                append_log(&log_file, "[shell] 引擎端口未就绪，仍尝试加载窗口\n".as_bytes());
            }
            let url =
                Url::parse(&format!("http://127.0.0.1:{port}/")).expect("引擎地址必须合法");
            // 自绘标题栏的控制服务：页面顶栏兼任窗口标题栏，按钮与拖拽经本机回环回传。
            let ctrl_port = spawn_window_control(handle.clone());
            // 只把端口号告诉页面；拿不到端口时页面保持原样，原生标题栏兜底。
            let init_script = format!("window.__PML_SHELL_CTRL={ctrl_port};");
            // setup 跑在主线程上，WebView 窗口必须在这里创建。
            let mut builder =
                WebviewWindowBuilder::new(&handle, "main", WebviewUrl::External(url))
                    .title("打流测试")
                    .inner_size(1280.0, 880.0)
                    .min_inner_size(960.0, 640.0)
                    .center()
                    .theme(Some(tauri::Theme::Dark));
            if ctrl_port > 0 {
                // 去掉原生标题栏，让窗口顶部直接是页面配色，Win10/Win11 观感一致。
                builder = builder.decorations(false).shadow(true);
            }
            let window = builder.initialization_script(init_script).build()?;
            tint_titlebar(&window);
            let _ = window.set_focus();
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("打流测试外壳启动失败");

    app.run(|app_handle, event| match event {
        RunEvent::Exit => {
            if let Some(state) = app_handle.try_state::<Engine>() {
                if let Ok(mut g) = state.0.lock() {
                    if let Some(child) = g.take() {
                        let _ = child.kill();
                    }
                }
            }
        }
        _ => {}
    });
}
