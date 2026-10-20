//! `mnemonic write` (one pass over sources), `mnemonic cache build`,
//! `mnemonic import-ch <export_dir>`. launchd schedules the passes; the
//! binary never loops.

use std::path::PathBuf;
use std::time::SystemTime;

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};

#[derive(Parser)]
#[command(name = "mnemonic")]
struct Cli {
    /// moraine config file; defaults to the usual moraine resolution.
    #[arg(long)]
    config: Option<PathBuf>,
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Rewrite every changed source file's parquet under this machine's prefix.
    Write,
    /// Build a new DuckDB cache from every machine's lake files.
    Cache {
        #[command(subcommand)]
        action: CacheAction,
    },
    /// Import ClickHouse parquet exports into machine=archive-ch.
    ImportCh { export_dir: PathBuf },
}

#[derive(Subcommand)]
enum CacheAction {
    Build,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .with_writer(std::io::stderr)
        .init();
    let cli = Cli::parse();
    let resolved = moraine_config::resolve_config_path(cli.config);
    let (_, app) = moraine_config::load_resolved_config(resolved)?;
    let lake = app
        .mnemonic
        .clone()
        .context("config has no [mnemonic] section")?;
    let lake_dir = PathBuf::from(moraine_config::expand_path(&lake.lake_dir));
    let cache_dir = PathBuf::from(moraine_config::expand_path(&lake.cache_dir));
    match cli.command {
        Command::Write => {
            let stats = mnemonic::scan::run_once(&app, SystemTime::now()).await?;
            println!("{stats:?}");
            if stats.failed > 0 {
                anyhow::bail!("{} source files failed", stats.failed);
            }
        }
        Command::Cache {
            action: CacheAction::Build,
        } => {
            let stats = mnemonic_repo::cache::build(&lake_dir, &cache_dir)?;
            println!("{stats:?}");
        }
        Command::ImportCh { export_dir } => {
            let stats = mnemonic::archive::import_exports(&export_dir, &lake_dir)?;
            println!("{stats:?}");
        }
    }
    Ok(())
}
