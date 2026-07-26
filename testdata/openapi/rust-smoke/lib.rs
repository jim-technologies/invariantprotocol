pub mod library {
    include!(concat!(env!("OUT_DIR"), "/library.v1.rs"));
}

pub type LibraryClient =
    library::library_service_client::LibraryServiceClient<tonic::transport::Channel>;

pub fn register_library_service<T>(
    server: &invariant::Server,
    implementation: T,
) -> Result<(), tonic::Status>
where
    T: library::library_service_server::LibraryService,
{
    library::register_library_service_server(server, implementation)
}
